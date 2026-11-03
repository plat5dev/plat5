//! The plan: what an apply would change, line by line with what changed field by
//! field, and the apply's precondition (docs/concepts/config.md#plan-and-apply).
//! It compares what readers are served: before is the head's stored bodies,
//! after the new config's, expanded by the running version. So after an
//! upgrade, a plan of unchanged files shows what the new version serves
//! differently. Pure but for identity's counts, so the store only locks, reads
//! and writes around it.

use std::collections::{BTreeMap, BTreeSet};

use serde::{Deserialize, Serialize};
use serde_json::{json, Map, Value};

use crate::apply::Served;
use crate::config::{RouteChecks, Users, DEFAULT_DELETION_GRACE};
use crate::counts::{Counts, CountsRequest, CountsResponse};
use crate::expand;
use crate::schema::FieldError;

#[derive(Clone, Copy, Debug, PartialEq, Eq, Serialize)]
#[serde(rename_all = "lowercase")]
pub enum Op {
    Add,
    Change,
    Remove,
}

/// Also the order of the plan's lines.
#[derive(Clone, Copy, Debug, PartialEq, Eq, PartialOrd, Ord, Serialize)]
#[serde(rename_all = "snake_case")]
pub enum What {
    Kind,
    Setting,
    Role,
    RateLimit,
    Cors,
    Service,
}

#[derive(Clone, Debug, PartialEq, Serialize)]
pub struct Change {
    pub op: Op,
    pub what: What,
    pub name: String,
    #[serde(skip_serializing_if = "Option::is_none")]
    pub before: Option<Value>,
    #[serde(skip_serializing_if = "Option::is_none")]
    pub after: Option<Value>,
    #[serde(skip_serializing_if = "Option::is_none")]
    pub routes: Option<Vec<RouteChange>>,
    #[serde(skip_serializing_if = "Option::is_none")]
    pub blocked: Option<Blocked>,
}

/// One expanded route of a service line, by method and public path.
#[derive(Clone, Debug, PartialEq, Serialize)]
pub struct RouteChange {
    pub op: Op,
    pub method: String,
    pub path: String,
    #[serde(skip_serializing_if = "Option::is_none")]
    pub before: Option<Value>,
    #[serde(skip_serializing_if = "Option::is_none")]
    pub after: Option<Value>,
}

#[derive(Clone, Debug, PartialEq, Serialize)]
pub struct Blocked {
    pub reason: String,
    pub in_the_way: InTheWay,
}

#[derive(Clone, Debug, PartialEq, Serialize)]
pub struct InTheWay {
    pub nodes: u64,
    pub sample: Vec<String>,
}

#[derive(Clone, Debug, Default, PartialEq, Eq, Serialize, Deserialize)]
pub struct Summary {
    pub add: u64,
    pub change: u64,
    pub remove: u64,
    pub blocked: u64,
}

#[derive(Clone, Debug, PartialEq, Serialize)]
pub struct Plan {
    pub revision: i64,
    pub summary: Summary,
    pub changes: Vec<Change>,
}

impl Plan {
    pub fn is_empty(&self) -> bool {
        self.changes.is_empty()
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
    /// **422**: a blocked plan, or no `expect`.
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

const KIND_NOT_EMPTY: &str = "kind_not_empty";

/// The dry run: the plan against the bodies served at `revision` (`None`
/// before the first apply). Writes nothing and ignores `expect`.
pub async fn dry_run(
    revision: i64,
    current: Option<&Served>,
    next: &Served,
    counts: &dyn Counts,
) -> Result<Plan, Refused> {
    make_plan(revision, current, next, counts).await
}

/// The apply: the same plan, recomputed, if its precondition holds and nothing
/// blocks it. The caller holds the head locked from reading `revision` until it
/// writes.
pub async fn apply(
    revision: i64,
    current: Option<&Served>,
    next: &Served,
    expect: Option<&Expect>,
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
    let blocked: Vec<FieldError> = plan
        .changes
        .iter()
        .filter_map(|c| {
            let b = c.blocked.as_ref()?;
            let (path, what) = match c.op {
                Op::Remove => (
                    format!("kinds.{}", c.name),
                    "nodes, live or deleted; delete them first and wait for the purge",
                ),
                _ => (
                    format!("kinds.{}.parent", c.name),
                    "nodes, live or deleted; a kind changes parent only while it has none",
                ),
            };
            Some(FieldError::new(
                path,
                format!(
                    "blocked ({}): {} has {} {what}, such as {}",
                    b.reason,
                    c.name,
                    b.in_the_way.nodes,
                    b.in_the_way.sample.join(", ")
                ),
            ))
        })
        .collect();
    if !blocked.is_empty() {
        return Err(Refused::Invalid(blocked));
    }
    Ok(plan)
}

/// A map of named things in a served body, each as served.
fn named(body: &Value, key: &str) -> BTreeMap<String, Value> {
    body.get(key)
        .and_then(Value::as_object)
        .map(|m| m.iter().map(|(k, v)| (k.clone(), v.clone())).collect())
        .unwrap_or_default()
}

async fn make_plan(
    revision: i64,
    current: Option<&Served>,
    next: &Served,
    counts: &dyn Counts,
) -> Result<Plan, Refused> {
    let old_kinds = current
        .map(|c| named(&c.gateway, "kinds"))
        .unwrap_or_default();
    let new_kinds = named(&next.gateway, "kinds");
    let parent = |k: &Value| k.get("parent").cloned();

    // What needs identity: kinds removed, and kinds under a new parent.
    let removed: BTreeSet<&String> = old_kinds
        .keys()
        .filter(|k| !new_kinds.contains_key(*k))
        .collect();
    let moved: BTreeSet<&String> = new_kinds
        .iter()
        .filter(|(k, kind)| old_kinds.get(*k).is_some_and(|o| parent(o) != parent(kind)))
        .map(|(k, _)| k)
        .collect();
    let request = CountsRequest {
        kinds: removed.union(&moved).map(|k| k.to_string()).collect(),
    };
    let counts = if request.kinds.is_empty() {
        CountsResponse::default()
    } else {
        counts
            .plan_counts(request)
            .await
            .map_err(Refused::CountsUnavailable)?
    };
    let count = |k: &str| counts.kinds.get(k).cloned().unwrap_or_default();

    let mut changes = Vec::new();

    for mut c in diff_map(What::Kind, &old_kinds, &new_kinds) {
        let needs_empty = match c.op {
            Op::Remove => true,
            Op::Change => moved.contains(&c.name),
            Op::Add => false,
        };
        let n = count(&c.name);
        c.blocked = (needs_empty && n.nodes > 0).then(|| Blocked {
            reason: KIND_NOT_EMPTY.into(),
            in_the_way: InTheWay {
                nodes: n.nodes,
                sample: n.sample,
            },
        });
        changes.push(c);
    }

    // Settings, with their defaults filled in. Before the first apply, a
    // setting at its default isn't a change.
    let old_settings = current.map_or_else(default_settings, settings);
    for (name, value) in settings(next) {
        match old_settings.get(&name) {
            Some(old) if *old == value => {}
            Some(old) if current.is_some() => changes.push(line(
                Op::Change,
                What::Setting,
                &name,
                Some(old),
                Some(&value),
            )),
            _ => changes.push(line(Op::Add, What::Setting, &name, None, Some(&value))),
        }
    }

    changes.extend(diff_map(
        What::Role,
        &current
            .map(|c| named(&c.identity, "roles"))
            .unwrap_or_default(),
        &named(&next.identity, "roles"),
    ));
    changes.extend(diff_map(
        What::RateLimit,
        &current
            .map(|c| named(&c.gateway, "rate_limits"))
            .unwrap_or_default(),
        &named(&next.gateway, "rate_limits"),
    ));

    let new_cors = next.gateway["cors"].clone();
    match current.map(|c| &c.gateway["cors"]) {
        Some(old) if *old == new_cors => {}
        Some(old) => changes.push(line(
            Op::Change,
            What::Cors,
            "cors",
            Some(old),
            Some(&new_cors),
        )),
        None if new_cors == json!({ "allowed_origins": [], "allowed_headers": [] }) => {}
        None => changes.push(line(Op::Add, What::Cors, "cors", None, Some(&new_cors))),
    }

    let old_services = current
        .map(|c| named(&c.gateway, "services"))
        .unwrap_or_default();
    let new_services = named(&next.gateway, "services");
    for (name, service) in &new_services {
        match old_services.get(name) {
            None => {
                let mut c = line(Op::Add, What::Service, name, None, Some(&own(service)));
                c.routes = Some(diff_routes(None, Some(service)));
                changes.push(c);
            }
            Some(old) => {
                let routes = diff_routes(Some(old), Some(service));
                let (before, after) = (own(old), own(service));
                if before == after && routes.is_empty() {
                    continue;
                }
                let mut c = if before == after {
                    line(Op::Change, What::Service, name, None, None)
                } else {
                    line(Op::Change, What::Service, name, Some(&before), Some(&after))
                };
                c.routes = Some(routes);
                changes.push(c);
            }
        }
    }
    for (name, old) in old_services
        .iter()
        .filter(|(s, _)| !new_services.contains_key(*s))
    {
        let mut c = line(Op::Remove, What::Service, name, Some(&own(old)), None);
        c.routes = Some(diff_routes(Some(old), None));
        changes.push(c);
    }

    changes.sort_by(|a, b| a.what.cmp(&b.what).then_with(|| a.name.cmp(&b.name)));
    Ok(Plan {
        revision,
        summary: summarize(&changes),
        changes,
    })
}

/// The settings by name, as served, with their defaults filled in.
fn settings(s: &Served) -> BTreeMap<String, Value> {
    let mut slugs: Vec<Value> = s.identity["reserved_slugs"]
        .as_array()
        .cloned()
        .unwrap_or_default();
    slugs.sort_by(|a, b| a.as_str().cmp(&b.as_str()));
    BTreeMap::from([
        (
            "deletion_grace".into(),
            s.identity["deletion_grace"].clone(),
        ),
        ("reserved_slugs".into(), Value::Array(slugs)),
        ("users".into(), s.gateway["users"].clone()),
        ("route_checks".into(), s.gateway["route_checks"].clone()),
        ("defaults".into(), s.gateway["defaults"].clone()),
        ("creator_role".into(), s.identity["creator_role"].clone()),
    ])
}

/// What each setting is before the first apply. `defaults` and `creator_role`
/// have none: every config states them.
fn default_settings() -> BTreeMap<String, Value> {
    BTreeMap::from([
        ("deletion_grace".into(), json!(DEFAULT_DELETION_GRACE)),
        ("reserved_slugs".into(), json!([])),
        ("users".into(), json!(Users::default())),
        ("route_checks".into(), json!(RouteChecks::default())),
    ])
}

/// A service's own fields, as the gateway is served them.
fn own(s: &Value) -> Value {
    json!({ "url": s["url"], "timeouts": s["timeouts"], "max_body_bytes": s["max_body_bytes"] })
}

/// A line. On a change, `before` and `after` keep only the fields that differ.
fn line(op: Op, what: What, name: &str, before: Option<&Value>, after: Option<&Value>) -> Change {
    let (before, after) = match (before, after) {
        (Some(b), Some(a)) => diff(b, a),
        (b, a) => (b.cloned(), a.cloned()),
    };
    Change {
        op,
        what,
        name: name.to_string(),
        before,
        after,
        routes: None,
        blocked: None,
    }
}

/// Of two objects, the fields that differ on each side (`None` when there are
/// none); anything else whole.
fn diff(before: &Value, after: &Value) -> (Option<Value>, Option<Value>) {
    let (Value::Object(b), Value::Object(a)) = (before, after) else {
        return (Some(before.clone()), Some(after.clone()));
    };
    let side = |this: &Map<String, Value>, other: &Map<String, Value>| {
        let m: Map<String, Value> = this
            .iter()
            .filter(|(k, v)| other.get(*k) != Some(*v))
            .map(|(k, v)| (k.clone(), v.clone()))
            .collect();
        (!m.is_empty()).then_some(Value::Object(m))
    };
    (side(b, a), side(a, b))
}

/// Lines for a map of named things: added, changed, removed.
fn diff_map(
    what: What,
    old: &BTreeMap<String, Value>,
    new: &BTreeMap<String, Value>,
) -> Vec<Change> {
    let mut out = Vec::new();
    for (name, value) in new {
        match old.get(name) {
            None => out.push(line(Op::Add, what, name, None, Some(value))),
            Some(o) if o != value => out.push(line(Op::Change, what, name, Some(o), Some(value))),
            Some(_) => {}
        }
    }
    for (name, value) in old.iter().filter(|(n, _)| !new.contains_key(*n)) {
        out.push(line(Op::Remove, what, name, Some(value), None));
    }
    out
}

/// A service's expanded routes that differ, by path and method.
fn diff_routes(old: Option<&Value>, new: Option<&Value>) -> Vec<RouteChange> {
    let rows = |s: Option<&Value>| -> BTreeMap<(String, usize, String), Value> {
        s.and_then(|s| s["routes"].as_array())
            .map_or(&[][..], Vec::as_slice)
            .iter()
            .map(|r| {
                let mut v = r.clone();
                let path = r["path"].as_str().unwrap_or_default().to_string();
                let method = r["method"].as_str().unwrap_or_default().to_string();
                if let Some(fields) = v.as_object_mut() {
                    fields.shift_remove("path");
                    fields.shift_remove("method");
                }
                ((path, expand::verb_order(&method), method), v)
            })
            .collect()
    };
    let (old, new) = (rows(old), rows(new));
    let keys: BTreeSet<&(String, usize, String)> = old.keys().chain(new.keys()).collect();
    keys.into_iter()
        .filter_map(|key| {
            let (op, before, after) = match (old.get(key), new.get(key)) {
                (None, Some(a)) => (Op::Add, None, Some(a.clone())),
                (Some(b), None) => (Op::Remove, Some(b.clone()), None),
                (Some(b), Some(a)) if b != a => {
                    let (b, a) = diff(b, a);
                    (Op::Change, b, a)
                }
                _ => return None,
            };
            Some(RouteChange {
                op,
                method: key.2.clone(),
                path: key.0.clone(),
                before,
                after,
            })
        })
        .collect()
}

/// Counts lines; a blocked line counts only in `blocked`.
fn summarize(changes: &[Change]) -> Summary {
    let mut s = Summary::default();
    for c in changes {
        match (&c.blocked, c.op) {
            (Some(_), _) => s.blocked += 1,
            (None, Op::Add) => s.add += 1,
            (None, Op::Change) => s.change += 1,
            (None, Op::Remove) => s.remove += 1,
        }
    }
    s
}

#[cfg(test)]
mod tests;
