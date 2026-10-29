//! One path for every apply: split off `expect`, check the schema and the
//! rules, plan against the head, and write if the precondition holds. Also what
//! the feeds serve.

use std::collections::BTreeMap;

use serde::Serialize;
use serde_json::{json, Map, Value};

use crate::config::{Config, APPLY_ONLY_KEYS};
use crate::counts::Counts;
use crate::expand::{self, ExpandedService};
use crate::plan::{self, Expect, Plan, Refused, Summary};
use crate::rules;
use crate::schema::{self, FieldError};
use crate::store::{Store, StoreError};

/// Why an apply or a dry run didn't happen.
#[derive(Debug)]
pub enum Failure {
    Invalid(Vec<FieldError>),
    Stale {
        precondition: String,
        expected: Option<i64>,
        actual: Option<i64>,
    },
    CountsUnavailable(String),
    Store(StoreError),
}

impl From<StoreError> for Failure {
    fn from(e: StoreError) -> Self {
        Failure::Store(e)
    }
}

impl From<Refused> for Failure {
    fn from(r: Refused) -> Self {
        match r {
            Refused::Invalid(e) => Failure::Invalid(e),
            Refused::Stale {
                precondition,
                expected,
                actual,
            } => Failure::Stale {
                precondition,
                expected,
                actual,
            },
            Refused::CountsUnavailable(e) => Failure::CountsUnavailable(e),
        }
    }
}

/// An apply body, checked: the config as sent, parsed, and the precondition.
pub struct Prepared {
    pub sent: Value,
    pub config: Config,
    pub expect: Option<Expect>,
}

pub fn prepare(body: Value) -> Result<Prepared, Failure> {
    let Value::Object(mut body) = body else {
        return Err(Failure::Invalid(vec![FieldError::new(
            "",
            "the config must be an object",
        )]));
    };
    let mut pre = Map::new();
    for key in APPLY_ONLY_KEYS {
        if let Some(v) = body.remove(*key) {
            pre.insert(key.to_string(), v);
        }
    }
    let expect = match pre.remove("expect") {
        None => None,
        Some(v) => Some(
            serde_json::from_value::<Expect>(v)
                .map_err(|e| Failure::Invalid(vec![FieldError::new("expect", e.to_string())]))?,
        ),
    };
    let sent = Value::Object(body);
    let config = check(&sent)?;
    Ok(Prepared {
        sent,
        config,
        expect,
    })
}

/// The schema, then the rules.
pub fn check(sent: &Value) -> Result<Config, Failure> {
    schema::check_config(sent).map_err(Failure::Invalid)?;
    let config: Config = serde_json::from_value(sent.clone())
        .map_err(|e| Failure::Invalid(vec![FieldError::new("", e.to_string())]))?;
    rules::check(&config).map_err(Failure::Invalid)?;
    Ok(config)
}

/// A stored revision's config. It passed every check when it was applied.
fn stored(config: &Value) -> Result<Config, Failure> {
    serde_json::from_value(config.clone())
        .map_err(|e| Failure::Store(StoreError(format!("stored config doesn't parse: {e}"))))
}

/// A dry run's answer: the plan, and every service of the planned config as
/// the gateway feed would serve it.
#[derive(Serialize)]
pub struct DryRun {
    #[serde(flatten)]
    pub plan: Plan,
    pub services: BTreeMap<String, ExpandedService>,
}

pub async fn dry_run(
    store: &Store,
    counts: &dyn Counts,
    prepared: &Prepared,
) -> Result<DryRun, Failure> {
    let current = store.current().await?;
    let (revision, config) = match &current {
        Some(r) => (r.revision, Some(stored(&r.config)?)),
        None => (store.head().await?, None),
    };
    let plan = plan::dry_run(revision, config.as_ref(), &prepared.config, counts).await?;
    Ok(DryRun {
        plan,
        services: expand::expand(&prepared.config),
    })
}

pub struct Applied {
    pub revision: i64,
    pub summary: Summary,
}

/// Applies with the head locked. An empty plan writes nothing and answers the
/// current revision.
pub async fn apply(
    store: &Store,
    counts: &dyn Counts,
    prepared: &Prepared,
    request_id: &str,
) -> Result<Applied, Failure> {
    let locked = store.lock().await?;
    let current = locked.config.as_ref().map(stored).transpose()?;
    let plan = plan::apply(
        locked.revision,
        current.as_ref(),
        &prepared.config,
        prepared.expect.as_ref(),
        counts,
    )
    .await?;
    if plan.is_empty() {
        return Ok(Applied {
            revision: locked.revision,
            summary: plan.summary,
        });
    }
    let summary = serde_json::to_value(&plan.summary).expect("summary serializes");
    let revision = store
        .write(locked, &prepared.sent, &summary, request_id)
        .await?;
    Ok(Applied {
        revision,
        summary: plan.summary,
    })
}

/// `GET /internal/gateway`: kinds, every service's routes expanded, rate
/// limits, CORS, `users` (the IdP name claims) and `route_checks`, with
/// defaults filled in.
pub fn gateway_feed(revision: i64, config: &Value) -> Result<Value, Failure> {
    let c = stored(config)?;
    Ok(gateway_body(revision, &c))
}

pub fn gateway_body(revision: i64, c: &Config) -> Value {
    json!({
        "revision": revision,
        "kinds": c.kinds,
        "services": expand::expand(c),
        "rate_limits": c.rate_limits,
        "cors": c.cors,
        "users": c.users,
        "route_checks": c.route_checks,
    })
}

/// `GET /internal/identity`: the settings, kinds and roles, with defaults
/// filled in. Roles as written, with their includes.
pub fn identity_feed(revision: i64, config: &Value) -> Result<Value, Failure> {
    let c = stored(config)?;
    Ok(identity_body(revision, &c))
}

pub fn identity_body(revision: i64, c: &Config) -> Value {
    json!({
        "revision": revision,
        "deletion_grace": c.deletion_grace,
        "reserved_slugs": c.reserved_slugs,
        "users": c.users,
        "kinds": c.kinds,
        "roles": c.roles,
        "creator_role": c.creator_role,
    })
}

#[cfg(test)]
mod tests {
    use super::*;

    fn base() -> Value {
        let path = std::path::Path::new(env!("CARGO_MANIFEST_DIR"))
            .join("fixtures/plans/first-apply/after.yml");
        let yaml: serde_yaml::Value =
            serde_yaml::from_str(&std::fs::read_to_string(path).unwrap()).unwrap();
        serde_json::to_value(yaml).unwrap()
    }

    #[test]
    fn prepare_splits_off_the_precondition() {
        let mut body = base();
        body["expect"] = json!({ "revision": 3 });
        let p = prepare(body).unwrap();
        assert_eq!(p.expect.unwrap().revision, 3);
        assert_eq!(p.sent, base());
    }

    #[test]
    fn prepare_names_bad_preconditions() {
        let mut body = base();
        body["expect"] = json!({ "rev": 3 });
        let Err(Failure::Invalid(errs)) = prepare(body) else {
            panic!()
        };
        assert_eq!(errs[0].path, "expect");
        let mut body = base();
        body["confirm"] = json!({ "folder": { "nodes": 3 } });
        let Err(Failure::Invalid(errs)) = prepare(body) else {
            panic!()
        };
        assert_eq!(errs[0].path, "");
    }

    #[test]
    fn feeds_fill_defaults_and_expand_routes() {
        let gateway = gateway_feed(4, &base()).unwrap();
        assert_eq!(gateway["revision"], json!(4));
        assert_eq!(
            gateway["cors"],
            json!({ "allowed_origins": [], "allowed_headers": [] })
        );
        assert_eq!(
            gateway["rate_limits"],
            json!({ "default": { "requests": 60, "window_seconds": 60 } })
        );
        let users = json!({ "name": { "claims": ["name", "preferred_username"], "sync": "first_sign_in" } });
        assert_eq!(gateway["users"], users);
        assert_eq!(gateway["route_checks"], json!({ "path": "/access/routes" }));
        let e10s = &gateway["services"]["e10s"];
        assert_eq!(e10s["timeouts"], json!({ "connect": "10s", "read": "30s" }));
        assert_eq!(e10s["max_body_bytes"], json!(10485760));
        assert_eq!(
            e10s["routes"][2],
            json!({
                "path": "/orgs/{organization}/projects/{project}/plans", "method": "GET",
                "scope": "project", "upstream": "/projects/{project.id}/plans",
                "required_permissions": ["project:read"], "rate_limit": ["default"], "audit": false,
                "allow_deleted": false
            })
        );
        assert_eq!(e10s["routes"][0]["required_permissions"], Value::Null);
        let identity = identity_feed(4, &base()).unwrap();
        assert_eq!(identity["deletion_grace"], json!("30d"));
        assert_eq!(identity["reserved_slugs"], json!([]));
        assert_eq!(identity["users"], users);
        assert!(identity.get("services").is_none());
        assert!(identity.get("route_checks").is_none());
    }

    #[test]
    fn users_name_fills_each_field_on_its_own() {
        let mut config = base();
        config["users"] = json!({ "name": { "sync": "each_sign_in" } });
        let want =
            json!({ "name": { "claims": ["name", "preferred_username"], "sync": "each_sign_in" } });
        assert_eq!(gateway_feed(1, &config).unwrap()["users"], want);
        assert_eq!(identity_feed(1, &config).unwrap()["users"], want);
        config["users"] = json!({ "name": { "claims": ["profile.name"] } });
        let want = json!({ "name": { "claims": ["profile.name"], "sync": "first_sign_in" } });
        assert_eq!(identity_feed(1, &config).unwrap()["users"], want);
    }
}
