//! What the schema can't say: the rules that tie the documents together
//! (docs/concepts/config.md#validation-at-apply) and the route rules
//! (docs/reference/config/services.md#validation), over every expanded route,
//! identity's and audit's included. The control plane is the one validator: the
//! gateway and identity trust what passes.

mod path;
mod routes;

use std::collections::{BTreeMap, BTreeSet};

use crate::config::{Config, Required};
use crate::expand::{self, ExpandedRoute, ExpandedService};
use crate::schema::{FieldError, MAX_ERRORS};

/// Upper bound on any duration (100 years in seconds), keeping values well
/// within Go's signed 64-bit nanosecond `time.Duration` (~292 years).
const MAX_DURATION_SECS: u64 = 36_500 * 86_400;

/// The validated, pre-expanded result of `rules::check`, reused directly by
/// `served::Served::of_checked` so routes and permissions are only expanded once.
#[derive(Clone, Debug)]
pub struct Checked {
    pub services: BTreeMap<String, ExpandedService>,
    pub route_permissions: BTreeSet<String>,
}

/// Every rule, over a config that already passed the schema. Each error names
/// a field path.
pub fn check(config: &Config) -> Result<Checked, Vec<FieldError>> {
    let mut errs = Vec::new();
    check_durations(config, &mut errs);
    check_kinds_tree(config, &mut errs);
    let mut routes_by_service: BTreeMap<String, Vec<ExpandedRoute>> = BTreeMap::new();
    let mut route_permissions = BTreeSet::new();
    // The rest reads the tree; with a broken tree its errors are noise.
    if errs.is_empty() {
        check_kinds_extras(config, &mut errs);
        check_policies(config, &mut errs);
        let mut valid = Vec::new();
        for spec in &expand::specs(config) {
            if routes::check_spec(config, spec, &mut errs) {
                if config.kinds.contains_key(&spec.scope) {
                    for verb in &spec.verbs {
                        if let Some(req) = &verb.permissions {
                            if let Required::Permissions(perms) = &req.value {
                                route_permissions.extend(perms.iter().cloned());
                            }
                        }
                    }
                }
                let expanded = expand::expand_spec(config, spec);
                valid.extend(expanded.iter().cloned());
                routes_by_service
                    .entry(spec.service.clone())
                    .or_default()
                    .extend(expanded);
            }
        }
        routes::check_shapes(&valid, &mut errs);
        routes::check_route_checks(config, &valid, &mut errs);
        check_authz_mode(config, &mut errs);
    }
    if errs.is_empty() {
        return Ok(Checked {
            services: expand::assemble_services(config, routes_by_service),
            route_permissions,
        });
    }
    let mut seen = BTreeSet::new();
    errs.retain(|e| seen.insert((e.path.clone(), e.message.clone())));
    errs.truncate(MAX_ERRORS);
    Err(errs)
}

fn check_duration(raw: &str, at: impl Into<String>, errs: &mut Vec<FieldError>) {
    let Some((num_part, unit)) = raw.split_at_checked(raw.len().saturating_sub(1)) else {
        return;
    };
    let mult: u64 = match unit {
        "s" => 1,
        "m" => 60,
        "h" => 3600,
        "d" => 86_400,
        _ => return,
    };
    let secs = num_part
        .parse::<u64>()
        .ok()
        .and_then(|n| n.checked_mul(mult));
    if secs.is_none_or(|s| s > MAX_DURATION_SECS) {
        errs.push(FieldError::new(
            at,
            format!("{raw:?} exceeds the maximum duration (36500d)"),
        ));
    }
}

fn check_durations(config: &Config, errs: &mut Vec<FieldError>) {
    check_duration(&config.deletion_grace, "deletion_grace", errs);
    if let Some(d) = &config.defaults.timeouts.connect {
        check_duration(d, "defaults.timeouts.connect", errs);
    }
    if let Some(d) = &config.defaults.timeouts.read {
        check_duration(d, "defaults.timeouts.read", errs);
    }
    if let Some(a) = &config.authorizer {
        check_duration(&a.timeout, "authorizer.timeout", errs);
        check_duration(&a.cache_ttl, "authorizer.cache_ttl", errs);
        check_duration(&a.negative_cache_ttl, "authorizer.negative_cache_ttl", errs);
    }
    for (name, p) in &config.rate_limits {
        check_duration(&p.window, format!("rate_limits.{name}.window"), errs);
        for (tier, tl) in &p.tiers {
            check_duration(
                &tl.window,
                format!("rate_limits.{name}.tiers.{tier}.window"),
                errs,
            );
        }
    }
    for (name, s) in &config.services {
        if let Some(d) = &s.timeouts.connect {
            check_duration(d, format!("services.{name}.timeouts.connect"), errs);
        }
        if let Some(d) = &s.timeouts.read {
            check_duration(d, format!("services.{name}.timeouts.read"), errs);
        }
    }
}

/// Structural tree checks: every parent is a kind, no cycles, and each kind's
/// path param is its own name. Failures here short-circuit the remaining rules.
fn check_kinds_tree(config: &Config, errs: &mut Vec<FieldError>) {
    for (name, kind) in &config.kinds {
        if let Some(parent) = &kind.parent {
            if !config.kinds.contains_key(parent) {
                errs.push(FieldError::new(
                    format!("kinds.{name}.parent"),
                    format!("{parent:?} is not a kind"),
                ));
            }
        }
        if kind.param() != name {
            errs.push(FieldError::new(
                format!("kinds.{name}.path"),
                format!(
                    "the param is the kind's name: {}/{{{name}}}",
                    kind.collection()
                ),
            ));
        }
    }
    let mut reported = BTreeSet::new();
    for name in config.kinds.keys() {
        let mut cycle = vec![name.as_str()];
        let mut at = name.as_str();
        while let Some(parent) = config.kinds.get(at).and_then(|k| k.parent.as_deref()) {
            if parent == name {
                cycle.push(parent);
                let first = cycle[..cycle.len() - 1].iter().min().expect("not empty");
                if reported.insert(first.to_string()) {
                    errs.push(FieldError::new(
                        format!("kinds.{first}.parent"),
                        format!("parents form a cycle: {}", cycle.join(" → ")),
                    ));
                }
                break;
            }
            if cycle.contains(&parent) || cycle.len() > config.kinds.len() {
                break;
            }
            cycle.push(parent);
            at = parent;
        }
    }
}

/// Non-structural kind checks (`host`, sibling collection uniqueness).
fn check_kinds_extras(config: &Config, errs: &mut Vec<FieldError>) {
    let mut host_suffixes: BTreeMap<String, String> = BTreeMap::new();
    let mut collections: BTreeMap<(Option<&str>, &str), &str> = BTreeMap::new();
    for (name, kind) in &config.kinds {
        let key = (kind.parent.as_deref(), kind.collection());
        if let Some(other) = collections.insert(key, name.as_str()) {
            errs.push(FieldError::new(
                format!("kinds.{name}.path"),
                format!(
                    "collection {:?} is already used by sibling kind {other:?}",
                    kind.collection()
                ),
            ));
        }
        if let Some((param, suffix)) = kind.host_parts() {
            if kind.parent.is_some() {
                errs.push(FieldError::new(
                    format!("kinds.{name}.host"),
                    "host is only allowed on root kinds",
                ));
            } else if param != name {
                errs.push(FieldError::new(
                    format!("kinds.{name}.host"),
                    format!("the host param must be the kind's name: {{{name}}}{suffix}"),
                ));
            } else if let Some(other) = host_suffixes.insert(suffix.to_string(), name.clone()) {
                errs.push(FieldError::new(
                    format!("kinds.{name}.host"),
                    format!("host suffix {suffix:?} is already used by kind {other:?}"),
                ));
            }
        }
    }
}

/// Rule 5, for the policies: a `count_at` is a kind, and each policy
/// `defaults.rate_limit` names is in `rate_limits` and has no `count_at`.
fn check_policies(config: &Config, errs: &mut Vec<FieldError>) {
    for (name, policy) in &config.rate_limits {
        if let Some(kind) = &policy.count_at {
            if !config.kinds.contains_key(kind) {
                errs.push(FieldError::new(
                    format!("rate_limits.{name}.count_at"),
                    format!("{kind:?} is not a kind"),
                ));
            }
        }
    }
    for (i, name) in config.defaults.rate_limit.iter().enumerate() {
        let at = format!("defaults.rate_limit[{i}]");
        match config.rate_limits.get(name) {
            None => errs.push(FieldError::new(
                at,
                format!("{name:?} is not a policy in rate_limits"),
            )),
            Some(p) if p.count_at.is_some() => errs.push(FieldError::new(
                at,
                format!("{name:?} counts at a kind: a default applies to user and public routes too, which have no node to count at"),
            )),
            Some(_) => {}
        }
    }
}

/// Rule 2: all-or-nothing authz mode — either built-in RBAC (`roles` +
/// `creator_role`) or external `authorizer`, mutually exclusive.
fn check_authz_mode(config: &Config, errs: &mut Vec<FieldError>) {
    if config.authorizer.is_some() {
        if !config.roles.is_empty() {
            errs.push(FieldError::new(
                "roles",
                "cannot be set when authorizer is configured",
            ));
        }
        if config.creator_role.is_some() {
            errs.push(FieldError::new(
                "creator_role",
                "cannot be set when authorizer is configured",
            ));
        }
        if config.custom_roles {
            errs.push(FieldError::new(
                "custom_roles",
                "cannot be set when authorizer is configured",
            ));
        }
        if !config.permissions.is_empty() {
            errs.push(FieldError::new(
                "permissions",
                "cannot be set when authorizer is configured",
            ));
        }
        return;
    }

    if config.roles.is_empty() {
        errs.push(FieldError::new(
            "roles",
            "required unless authorizer is configured",
        ));
    }
    match config.creator_role.as_deref() {
        None => errs.push(FieldError::new(
            "creator_role",
            "required unless authorizer is configured",
        )),
        Some(creator_role) => {
            if !config.roles.is_empty() {
                match config.roles.get(creator_role) {
                    None => errs.push(FieldError::new(
                        "creator_role",
                        format!("{creator_role:?} is not a role"),
                    )),
                    Some(role) => {
                        let missing: Vec<&str> = config
                            .kinds
                            .keys()
                            .filter(|k| !role.bindable_at.contains(k))
                            .map(String::as_str)
                            .collect();
                        if !missing.is_empty() {
                            errs.push(FieldError::new(
                                "creator_role",
                                format!(
                                    "{creator_role:?} must be bindable at every kind; it isn't at {}",
                                    missing.join(", ")
                                ),
                            ));
                        }
                    }
                }
            }
        }
    }

    for (slug, role) in &config.roles {
        for (i, kind) in role.bindable_at.iter().enumerate() {
            if !config.kinds.contains_key(kind) {
                errs.push(FieldError::new(
                    format!("roles.{slug}.bindable_at[{i}]"),
                    format!("{kind:?} is not a kind"),
                ));
            }
        }
        for (i, included) in role.includes.iter().enumerate() {
            if !config.roles.contains_key(included) {
                errs.push(FieldError::new(
                    format!("roles.{slug}.includes[{i}]"),
                    format!("{included:?} is not a role"),
                ));
            }
        }
    }
    if let Some(cycle) = include_cycle(config) {
        errs.push(FieldError::new(
            format!("roles.{}.includes", cycle[0]),
            format!("includes form a cycle: {}", cycle.join(" → ")),
        ));
    }
    check_permissions(config, errs);
}

fn check_permissions(config: &Config, errs: &mut Vec<FieldError>) {
    for (perm, meta) in &config.permissions {
        for (i, kind) in meta.bindable_at.iter().enumerate() {
            if !config.kinds.contains_key(kind) {
                errs.push(FieldError::new(
                    format!("permissions.{perm}.bindable_at[{i}]"),
                    format!("{kind:?} is not a kind"),
                ));
            }
        }
    }
}

fn include_cycle(config: &Config) -> Option<Vec<String>> {
    fn visit(
        config: &Config,
        role: &str,
        stack: &mut Vec<String>,
        done: &mut BTreeSet<String>,
    ) -> Option<Vec<String>> {
        if let Some(at) = stack.iter().position(|r| r == role) {
            let mut cycle = stack[at..].to_vec();
            cycle.push(role.to_string());
            return Some(cycle);
        }
        if !done.insert(role.to_string()) {
            return None;
        }
        stack.push(role.to_string());
        for included in config.roles.get(role).map_or(&[][..], |r| &r.includes) {
            if let Some(c) = visit(config, included, stack, done) {
                return Some(c);
            }
        }
        stack.pop();
        None
    }
    let mut done = BTreeSet::new();
    config
        .roles
        .keys()
        .find_map(|r| visit(config, r, &mut Vec::new(), &mut done))
}

#[cfg(test)]
mod tests;
