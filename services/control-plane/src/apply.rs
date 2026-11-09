//! One path for every apply: split off `expect`, check the schema and the
//! rules, build the served bodies, plan the head's stored bodies against the
//! new ones, and write the source and the bodies if the precondition holds. The feeds serve
//! the stored bodies; nothing here runs on a feed read.

use serde::Serialize;
use serde_json::{Map, Value};

use crate::config::{Config, APPLY_ONLY_KEYS};
use crate::counts::Counts;
use crate::plan::{self, Expect, Plan, Refused, Summary};
use crate::rules;
use crate::schema::{self, FieldError};
use crate::served::Served;
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
    let config = read(sent)?;
    rules::check(&config).map_err(Failure::Invalid)?;
    Ok(config)
}

/// A config read strictly: the schema, then the types, both refusing any field
/// they don't know (docs/concepts/config.md#the-config). Also how a stored
/// source is read for a pull, so one the running version can't read fails
/// loudly instead of losing a field.
pub fn read(sent: &Value) -> Result<Config, Failure> {
    schema::check_config(sent).map_err(Failure::Invalid)?;
    serde_json::from_value(sent.clone())
        .map_err(|e| Failure::Invalid(vec![FieldError::new("", e.to_string())]))
}

/// A dry run's answer: the plan, and every service of the planned config as
/// the gateway feed would serve it.
#[derive(Serialize)]
pub struct DryRun {
    #[serde(flatten)]
    pub plan: Plan,
    pub services: Value,
}

pub async fn dry_run(
    store: &Store,
    counts: &dyn Counts,
    prepared: &Prepared,
) -> Result<DryRun, Failure> {
    let (revision, current) = store.served().await?;
    let next = Served::of(&prepared.config);
    let plan = plan::dry_run(revision, current.as_ref(), &next, counts).await?;
    Ok(DryRun {
        plan,
        services: next.gateway["services"].clone(),
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
    let next = Served::of(&prepared.config);
    let plan = plan::apply(
        locked.revision,
        locked.served.as_ref(),
        &next,
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
        .write(locked, &prepared.sent, &next, &summary, request_id)
        .await?;
    Ok(Applied {
        revision,
        summary: plan.summary,
    })
}

#[cfg(test)]
mod tests {
    use super::*;
    use serde_json::json;

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

    // Unknown fields fail wherever a config is read, nested ones included.
    #[test]
    fn unknown_fields_fail() {
        for (at, set) in [
            ("top", json!({ "bogus": 1 })),
            (
                "kind",
                json!({ "kinds": { "organization": { "path": "/orgs/{organization}", "permission_name": "org" } } }),
            ),
            (
                "defaults",
                json!({ "defaults": { "rate_limit": ["standard"], "burst": 1 } }),
            ),
        ] {
            let mut config = base();
            for (k, v) in set.as_object().unwrap() {
                config[k] = v.clone();
            }
            assert!(
                matches!(read(&config), Err(Failure::Invalid(_))),
                "{at}: read"
            );
            assert!(matches!(check(&config), Err(Failure::Invalid(_))), "{at}");
        }
    }
}
