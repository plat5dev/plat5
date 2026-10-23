//! The plan: what an apply would change, each change classified, and the apply's
//! preconditions (docs/concepts/config.md#plan-and-apply). Pure but for identity's
//! counts, so the store only locks, reads and writes around it.

use std::collections::{BTreeMap, BTreeSet};

use serde::{Deserialize, Serialize};
use serde_json::{json, Value};

use crate::config::{Config, Service};
use crate::counts::{Counts, CountsRequest, Effects, InTheWay, Narrowed};
use crate::schema::FieldError;

#[derive(Clone, Copy, Debug, PartialEq, Eq, Serialize)]
#[serde(rename_all = "lowercase")]
pub enum Class {
    Safe,
    Blocked,
    Destructive,
}

#[derive(Clone, Copy, Debug, PartialEq, Eq, Serialize)]
#[serde(rename_all = "lowercase")]
pub enum Op {
    Add,
    Change,
    Remove,
}

#[derive(Clone, Copy, Debug, PartialEq, Eq, PartialOrd, Ord, Serialize)]
#[serde(rename_all = "lowercase")]
pub enum What {
    Kind,
    Setting,
    Role,
    Cors,
    Service,
}

#[derive(Clone, Debug, Default, PartialEq, Eq, Serialize, Deserialize)]
pub struct RouteCounts {
    pub add: u64,
    pub change: u64,
    pub remove: u64,
}

#[derive(Clone, Debug, PartialEq, Serialize)]
pub struct Change {
    pub class: Class,
    pub op: Op,
    pub what: What,
    pub name: String,
    #[serde(skip_serializing_if = "Option::is_none")]
    pub routes: Option<RouteCounts>,
    #[serde(skip_serializing_if = "Option::is_none")]
    pub reason: Option<String>,
    #[serde(skip_serializing_if = "Option::is_none")]
    pub in_the_way: Option<InTheWay>,
    #[serde(skip_serializing_if = "Option::is_none")]
    pub effects: Option<Effects>,
    /// A changed service's own `url`, `timeouts` or `rate_limits`: one change in
    /// the summary on top of its routes.
    #[serde(skip)]
    own_fields_changed: bool,
}

impl Change {
    fn new(class: Class, op: Op, what: What, name: &str) -> Self {
        Self {
            class,
            op,
            what,
            name: name.to_string(),
            routes: None,
            reason: None,
            in_the_way: None,
            effects: None,
            own_fields_changed: false,
        }
    }

    fn safe(op: Op, what: What, name: &str) -> Self {
        Self::new(Class::Safe, op, what, name)
    }
}

#[derive(Clone, Debug, Default, PartialEq, Eq, Serialize, Deserialize)]
pub struct Summary {
    pub add: u64,
    pub change: u64,
    pub remove: u64,
    pub blocked: u64,
}

/// What an apply must send to allow a destructive change: the node count of
/// each removed kind.
#[derive(Clone, Debug, PartialEq, Eq, Serialize, Deserialize)]
#[serde(deny_unknown_fields)]
pub struct Confirmed {
    pub nodes: u64,
}

#[derive(Clone, Debug, PartialEq, Serialize)]
pub struct Plan {
    pub revision: i64,
    pub summary: Summary,
    pub changes: Vec<Change>,
    pub confirm: BTreeMap<String, Confirmed>,
}

impl Plan {
    pub fn is_empty(&self) -> bool {
        self.changes.is_empty()
    }

    fn blocked(&self) -> impl Iterator<Item = &Change> {
        self.changes.iter().filter(|c| c.class == Class::Blocked)
    }
}

#[derive(Clone, Debug, Deserialize)]
#[serde(deny_unknown_fields)]
pub struct Expect {
    pub revision: i64,
}

/// Why a plan can't be made, or an apply is refused.
#[derive(Debug, PartialEq)]
pub enum Refused {
    /// **422**: a change the config can't make, a blocked plan, or no `expect`.
    Invalid(Vec<FieldError>),
    /// **409** `PLAN_STALE`.
    Stale {
        precondition: String,
        expected: Option<i64>,
        actual: Option<i64>,
    },
    /// **503**: the plan needs identity's counts and identity didn't answer.
    CountsUnavailable(String),
}

/// The dry run: the plan against the config at `revision` (`None` before the
/// first apply). Writes nothing and ignores `expect` and `confirm`.
pub async fn dry_run(
    revision: i64,
    current: Option<&Config>,
    next: &Config,
    counts: &dyn Counts,
) -> Result<Plan, Refused> {
    make_plan(revision, current, next, counts).await
}

/// The apply: the same plan, recomputed, if its preconditions hold. The caller
/// holds the head locked from reading `revision` until it writes.
pub async fn apply(
    revision: i64,
    current: Option<&Config>,
    next: &Config,
    expect: Option<&Expect>,
    confirm: Option<&BTreeMap<String, Confirmed>>,
    counts: &dyn Counts,
) -> Result<Plan, Refused> {
    let Some(expect) = expect else {
        return Err(Refused::Invalid(vec![FieldError::new(
            "expect",
            "required: the revision the plan was made against (0 for the first apply)",
        )]));
    };
    if expect.revision != revision {
        return Err(Refused::Stale {
            precondition: "revision".into(),
            expected: Some(expect.revision),
            actual: Some(revision),
        });
    }
    let plan = make_plan(revision, current, next, counts).await?;
    let empty = BTreeMap::new();
    let confirm = confirm.unwrap_or(&empty);
    for (kind, want) in &plan.confirm {
        let sent = confirm.get(kind);
        if sent != Some(want) {
            return Err(Refused::Stale {
                precondition: format!("confirm.{kind}"),
                expected: sent.map(|c| c.nodes as i64),
                actual: Some(want.nodes as i64),
            });
        }
    }
    if let Some(kind) = confirm.keys().find(|k| !plan.confirm.contains_key(*k)) {
        return Err(Refused::Stale {
            precondition: format!("confirm.{kind}"),
            expected: Some(confirm[kind].nodes as i64),
            actual: None,
        });
    }
    let blocked: Vec<FieldError> = plan
        .blocked()
        .map(|c| {
            let nodes = c.in_the_way.as_ref().map_or(0, |w| w.nodes);
            FieldError::new(
                format!("kinds.{}.parent", c.name),
                format!("blocked: {nodes} nodes are under a parent this config drops"),
            )
        })
        .collect();
    if !blocked.is_empty() {
        return Err(Refused::Invalid(blocked));
    }
    Ok(plan)
}

async fn make_plan(
    revision: i64,
    current: Option<&Config>,
    next: &Config,
    counts: &dyn Counts,
) -> Result<Plan, Refused> {
    check_transitions(current, next)?;

    let empty_kinds = BTreeMap::new();
    let old_kinds = current.map_or(&empty_kinds, |c| &c.kinds);

    // What needs identity: removed kinds, and parents dropped from a kind.
    let mut request = CountsRequest::default();
    for name in old_kinds.keys() {
        if !next.kinds.contains_key(name) {
            request.kinds_removed.push(name.clone());
        }
    }
    for (name, kind) in &next.kinds {
        if let Some(old) = old_kinds.get(name) {
            let now: BTreeSet<String> = kind.parents().into_iter().collect();
            let removed: Vec<String> = old
                .parents()
                .into_iter()
                .filter(|p| !now.contains(p))
                .collect();
            if !removed.is_empty() {
                request.parents_narrowed.push(Narrowed {
                    kind: name.clone(),
                    removed,
                });
            }
        }
    }
    let counts = if request.kinds_removed.is_empty() && request.parents_narrowed.is_empty() {
        Default::default()
    } else {
        counts
            .plan_counts(request)
            .await
            .map_err(Refused::CountsUnavailable)?
    };

    let mut changes = Vec::new();
    let mut confirm = BTreeMap::new();

    // Kinds.
    for (name, kind) in &next.kinds {
        match old_kinds.get(name) {
            None => changes.push(Change::safe(Op::Add, What::Kind, name)),
            Some(old) if old == kind => {}
            Some(old) => {
                let narrowed = old.parents().iter().any(|p| !kind.parents().contains(p));
                let in_the_way = counts.parents_narrowed.get(name).filter(|w| w.nodes > 0);
                match (narrowed, in_the_way) {
                    (true, Some(w)) => {
                        let mut c = Change::new(Class::Blocked, Op::Change, What::Kind, name);
                        c.reason = Some("parent_narrowed".into());
                        c.in_the_way = Some(w.clone());
                        changes.push(c);
                    }
                    _ => changes.push(Change::safe(Op::Change, What::Kind, name)),
                }
            }
        }
    }
    for name in old_kinds.keys().filter(|k| !next.kinds.contains_key(*k)) {
        let effects = counts.kinds_removed.get(name).cloned().unwrap_or_default();
        confirm.insert(
            name.clone(),
            Confirmed {
                nodes: effects.nodes,
            },
        );
        let mut c = Change::new(Class::Destructive, Op::Remove, What::Kind, name);
        c.effects = Some(effects);
        changes.push(c);
    }

    // Settings: always present once applied, with their defaults.
    let settings = |c: &Config| -> Vec<(&'static str, Value)> {
        vec![
            ("deletion_grace", json!(c.deletion_grace)),
            ("reserved_slugs", json!(c.reserved_slug_set())),
            ("default_role", json!(c.default_role)),
            ("creator_role", json!(c.creator_role)),
        ]
    };
    let old_settings = current.map(settings);
    for (i, (name, value)) in settings(next).into_iter().enumerate() {
        match &old_settings {
            None => {
                // Nothing to say about an empty list before the first apply.
                if value != json!([]) {
                    changes.push(Change::safe(Op::Add, What::Setting, name));
                }
            }
            Some(old) if old[i].1 != value => {
                changes.push(Change::safe(Op::Change, What::Setting, name))
            }
            Some(_) => {}
        }
    }

    // Roles.
    let empty_roles = BTreeMap::new();
    let old_roles = current.map_or(&empty_roles, |c| &c.roles);
    for (name, role) in &next.roles {
        match old_roles.get(name) {
            None => changes.push(Change::safe(Op::Add, What::Role, name)),
            Some(old) if old != role => changes.push(Change::safe(Op::Change, What::Role, name)),
            Some(_) => {}
        }
    }
    for name in old_roles.keys().filter(|r| !next.roles.contains_key(*r)) {
        changes.push(Change::safe(Op::Remove, What::Role, name));
    }

    // CORS: present when it lists an origin.
    let old_origins = current
        .map(|c| c.cors.allowed_origins.clone())
        .unwrap_or_default();
    let new_origins = &next.cors.allowed_origins;
    let op = match (old_origins.is_empty(), new_origins.is_empty()) {
        (true, false) => Some(Op::Add),
        (false, true) => Some(Op::Remove),
        (false, false) if old_origins != *new_origins => Some(Op::Change),
        _ => None,
    };
    if let Some(op) = op {
        changes.push(Change::safe(op, What::Cors, "allowed_origins"));
    }

    // Services, compared by their stored form, one row per path and method.
    let empty_services = BTreeMap::new();
    let old_services = current.map_or(&empty_services, |c| &c.services);
    for (name, service) in &next.services {
        match old_services.get(name) {
            None => {
                let mut c = Change::safe(Op::Add, What::Service, name);
                c.routes = Some(RouteCounts {
                    add: service.method_rows().len() as u64,
                    ..Default::default()
                });
                changes.push(c);
            }
            Some(old) => {
                let routes = diff_routes(old, service);
                let own = own_fields(old) != own_fields(service);
                if own || routes != RouteCounts::default() {
                    let mut c = Change::safe(Op::Change, What::Service, name);
                    c.routes = Some(routes);
                    c.own_fields_changed = own;
                    changes.push(c);
                }
            }
        }
    }
    for (name, old) in old_services
        .iter()
        .filter(|(s, _)| !next.services.contains_key(*s))
    {
        let mut c = Change::safe(Op::Remove, What::Service, name);
        c.routes = Some(RouteCounts {
            remove: old.method_rows().len() as u64,
            ..Default::default()
        });
        changes.push(c);
    }

    changes.sort_by(|a, b| a.what.cmp(&b.what).then_with(|| a.name.cmp(&b.name)));
    Ok(Plan {
        revision,
        summary: summarize(&changes),
        changes,
        confirm,
    })
}

/// Changes the config can't make at all (**422**), whatever is in identity.
fn check_transitions(current: Option<&Config>, next: &Config) -> Result<(), Refused> {
    let Some(current) = current else {
        return Ok(());
    };
    let mut errs = Vec::new();
    if let (Some(old), Some(new)) = (current.sign_in_kind(), next.sign_in_kind()) {
        if old != new {
            errs.push(FieldError::new(
                format!("kinds.{new}.sign_in"),
                format!("sign_in can't move from {old} to {new}: the sign-in node holds every person's directory member"),
            ));
        }
    }
    for (name, kind) in &next.kinds {
        let Some(old) = current.kinds.get(name) else {
            continue;
        };
        if old.is_root() != kind.is_root() {
            let what = if old.is_root() {
                "a root kind can't be given a parent"
            } else {
                "a kind under parents can't become a root"
            };
            errs.push(FieldError::new(
                format!("kinds.{name}.parent"),
                format!("{what}; remove {name} and add a new kind"),
            ));
        }
    }
    if errs.is_empty() {
        Ok(())
    } else {
        Err(Refused::Invalid(errs))
    }
}

fn own_fields(s: &Service) -> Value {
    json!({ "url": s.url, "timeouts": s.timeouts, "rate_limits": s.rate_limits })
}

fn diff_routes(old: &Service, new: &Service) -> RouteCounts {
    let (old, new) = (old.method_rows(), new.method_rows());
    let mut counts = RouteCounts::default();
    for (key, row) in &new {
        match old.get(key) {
            None => counts.add += 1,
            Some(o) if o != row => counts.change += 1,
            Some(_) => {}
        }
    }
    counts.remove = old.keys().filter(|k| !new.contains_key(*k)).count() as u64;
    counts
}

/// A kind, role, setting or CORS line counts one; a service counts its routes,
/// plus one change for its own fields; a blocked line counts only in `blocked`.
fn summarize(changes: &[Change]) -> Summary {
    let mut s = Summary::default();
    for c in changes {
        if c.class == Class::Blocked {
            s.blocked += 1;
            continue;
        }
        match (&c.routes, c.op) {
            (Some(r), _) => {
                s.add += r.add;
                s.change += r.change + u64::from(c.own_fields_changed);
                s.remove += r.remove;
            }
            (None, Op::Add) => s.add += 1,
            (None, Op::Change) => s.change += 1,
            (None, Op::Remove) => s.remove += 1,
        }
    }
    s
}

#[cfg(test)]
mod tests;
