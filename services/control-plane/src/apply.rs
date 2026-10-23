//! One path for every apply, from the admin API or the seed: split off `expect`
//! and `confirm`, check the schema and the rules, plan against the head, and
//! write if the preconditions hold. Also what the feeds serve.

use std::collections::BTreeMap;

use serde_json::{json, Map, Value};

use crate::config::{Config, APPLY_ONLY_KEYS};
use crate::counts::Counts;
use crate::plan::{self, Confirmed, Expect, Plan, Refused, Summary};
use crate::rules::{self, RuleError};
use crate::schema::{self, FieldError};
use crate::store::{Store, StoreError};

/// Why an apply or a dry run didn't happen.
#[derive(Debug)]
pub enum Failure {
    Invalid(Vec<FieldError>),
    RouteConflict(Vec<String>),
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

/// An apply body, checked: the config as sent, parsed, and the preconditions.
pub struct Prepared {
    pub sent: Value,
    pub config: Config,
    pub expect: Option<Expect>,
    pub confirm: Option<BTreeMap<String, Confirmed>>,
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
    let field = |key: &str, e: serde_json::Error| {
        Failure::Invalid(vec![FieldError::new(key, e.to_string())])
    };
    let expect = match pre.remove("expect") {
        None => None,
        Some(v) => Some(serde_json::from_value::<Expect>(v).map_err(|e| field("expect", e))?),
    };
    let confirm = match pre.remove("confirm") {
        None => None,
        Some(v) => Some(
            serde_json::from_value::<BTreeMap<String, Confirmed>>(v)
                .map_err(|e| field("confirm", e))?,
        ),
    };
    let sent = Value::Object(body);
    let config = check(&sent)?;
    Ok(Prepared {
        sent,
        config,
        expect,
        confirm,
    })
}

/// The schema, then the rules.
pub fn check(sent: &Value) -> Result<Config, Failure> {
    schema::check_config(sent).map_err(Failure::Invalid)?;
    let config: Config = serde_json::from_value(sent.clone())
        .map_err(|e| Failure::Invalid(vec![FieldError::new("", e.to_string())]))?;
    rules::check(&config).map_err(|e| match e {
        RuleError::Invalid(errs) => Failure::Invalid(errs),
        RuleError::RouteConflict(c) => Failure::RouteConflict(c),
    })?;
    Ok(config)
}

/// A stored revision's config. It passed every check when it was applied.
fn stored(config: &Value) -> Result<Config, Failure> {
    serde_json::from_value(config.clone())
        .map_err(|e| Failure::Store(StoreError(format!("stored config doesn't parse: {e}"))))
}

pub async fn dry_run(
    store: &Store,
    counts: &dyn Counts,
    prepared: &Prepared,
) -> Result<Plan, Failure> {
    let current = store.current().await?;
    let (revision, config) = match &current {
        Some(r) => (r.revision, Some(stored(&r.config)?)),
        None => (store.head().await?, None),
    };
    Ok(plan::dry_run(revision, config.as_ref(), &prepared.config, counts).await?)
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
        prepared.confirm.as_ref(),
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

/// `GET /internal/gateway`: kinds, services in the stored form, and CORS.
pub fn gateway_feed(revision: i64, config: &Value) -> Result<Value, Failure> {
    let c = stored(config)?;
    Ok(json!({
        "revision": revision,
        "kinds": c.kinds,
        "services": c.stored_services(),
        "cors": c.cors,
    }))
}

/// `GET /internal/identity`: the settings, kinds, roles and defaults, with
/// defaults filled in, and services in the stored form.
pub fn identity_feed(revision: i64, config: &Value) -> Result<Value, Failure> {
    let c = stored(config)?;
    Ok(json!({
        "revision": revision,
        "deletion_grace": c.deletion_grace,
        "reserved_slugs": c.reserved_slugs,
        "kinds": c.kinds,
        "roles": c.roles,
        "default_role": c.default_role,
        "creator_role": c.creator_role,
        "services": c.stored_services(),
    }))
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
    fn prepare_splits_off_the_preconditions() {
        let mut body = base();
        body["expect"] = json!({ "revision": 3 });
        body["confirm"] = json!({ "folder": { "nodes": 14 } });
        let p = prepare(body).unwrap();
        assert_eq!(p.expect.unwrap().revision, 3);
        assert_eq!(p.confirm.unwrap()["folder"], Confirmed { nodes: 14 });
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
    }

    #[test]
    fn feeds_fill_defaults_and_store_routes_expanded() {
        let gateway = gateway_feed(4, &base()).unwrap();
        assert_eq!(gateway["revision"], json!(4));
        assert_eq!(gateway["cors"], json!({ "allowed_origins": [] }));
        let routes = gateway["services"]["e10s"]["routes"].as_array().unwrap();
        // The methods map became one route per verb, each a list.
        assert_eq!(routes.len(), 3);
        assert_eq!(routes[0]["methods"], json!(["GET"]));
        assert_eq!(routes[0]["required_labels"], json!(["project:read"]));
        let identity = identity_feed(4, &base()).unwrap();
        assert_eq!(identity["deletion_grace"], json!("30d"));
        assert_eq!(identity["reserved_slugs"], json!([]));
        assert_eq!(identity["default_role"], json!("member"));
        assert!(identity.get("cors").is_none());
    }
}
