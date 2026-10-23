//! Fixture configs in `fixtures/plans/<case>/`:
//!
//! - `before.yml`: the applied config; absent before the first apply
//! - `after.yml`: the config planned
//! - `counts.json`: what identity answers, if the plan asks
//! - `plan.json`: the expected plan, or `refused.json`: the field paths it's
//!   refused at
//!
//! Every config passes the schema and the rules first, as at apply.

use std::path::{Path, PathBuf};

use serde_json::{json, Value};

use super::*;
use crate::counts::{CountsResponse, FixedCounts, NoCounts};

fn fixtures() -> PathBuf {
    Path::new(env!("CARGO_MANIFEST_DIR")).join("fixtures/plans")
}

pub(crate) fn load(path: &Path) -> Config {
    let text = std::fs::read_to_string(path).unwrap_or_else(|e| panic!("{}: {e}", path.display()));
    let yaml: serde_yaml::Value = serde_yaml::from_str(&text).unwrap();
    let value: Value = serde_json::to_value(yaml).unwrap();
    crate::schema::check_config(&value)
        .unwrap_or_else(|e| panic!("{}: schema: {e:?}", path.display()));
    let config: Config = serde_json::from_value(value).unwrap();
    crate::rules::check(&config).unwrap_or_else(|e| panic!("{}: rules: {e:?}", path.display()));
    config
}

fn read_json(path: &Path) -> Option<Value> {
    let text = std::fs::read_to_string(path).ok()?;
    Some(serde_json::from_str(&text).unwrap_or_else(|e| panic!("{}: {e}", path.display())))
}

#[tokio::test]
async fn fixture_configs_classify() {
    let mut cases: Vec<PathBuf> = std::fs::read_dir(fixtures())
        .unwrap()
        .map(|e| e.unwrap().path())
        .filter(|p| p.is_dir())
        .collect();
    cases.sort();
    assert!(cases.len() >= 10, "fixtures missing: {cases:?}");
    for case in cases {
        let name = case.file_name().unwrap().to_string_lossy().to_string();
        let before = case.join("before.yml");
        let before = before.exists().then(|| load(&before));
        let after = load(&case.join("after.yml"));
        let counts: CountsResponse = read_json(&case.join("counts.json"))
            .map(|v| serde_json::from_value(v).unwrap())
            .unwrap_or_default();
        let revision = if before.is_some() { 7 } else { 0 };
        let got = dry_run(revision, before.as_ref(), &after, &FixedCounts::new(counts)).await;

        if let Some(want) = read_json(&case.join("refused.json")) {
            let Err(Refused::Invalid(errs)) = got else {
                panic!("{name}: want refused, got {got:?}");
            };
            let paths: Vec<&str> = errs.iter().map(|e| e.path.as_str()).collect();
            assert_eq!(json!(paths), want, "{name}: {errs:?}");
            continue;
        }
        let plan = got.unwrap_or_else(|e| panic!("{name}: {e:?}"));
        let want =
            read_json(&case.join("plan.json")).unwrap_or_else(|| panic!("{name}: no plan.json"));
        assert_eq!(
            serde_json::to_value(&plan).unwrap(),
            want,
            "{name}: got {}",
            serde_json::to_string_pretty(&plan).unwrap()
        );
    }
}

fn case(name: &str) -> (Config, Config, CountsResponse) {
    let dir = fixtures().join(name);
    let counts = read_json(&dir.join("counts.json"))
        .map(|v| serde_json::from_value(v).unwrap())
        .unwrap_or_default();
    (
        load(&dir.join("before.yml")),
        load(&dir.join("after.yml")),
        counts,
    )
}

fn confirm(kind: &str, nodes: u64) -> BTreeMap<String, Confirmed> {
    BTreeMap::from([(kind.to_string(), Confirmed { nodes })])
}

#[tokio::test]
async fn a_confirmed_destructive_plan_applies() {
    let (before, after, counts) = case("remove-kind");
    let counts = FixedCounts::new(counts);
    let plan = dry_run(7, Some(&before), &after, &counts).await.unwrap();
    assert_eq!(plan.confirm, confirm("folder", 14));
    let applied = apply(
        7,
        Some(&before),
        &after,
        Some(&Expect { revision: 7 }),
        Some(&plan.confirm),
        &counts,
    )
    .await
    .unwrap();
    assert_eq!(applied, plan);
}

#[tokio::test]
async fn a_stale_expect_is_refused_before_counting() {
    let (before, after, counts) = case("remove-kind");
    let counts = FixedCounts::new(counts);
    let got = apply(
        8,
        Some(&before),
        &after,
        Some(&Expect { revision: 7 }),
        Some(&confirm("folder", 14)),
        &counts,
    )
    .await;
    assert_eq!(
        got,
        Err(Refused::Stale {
            precondition: "revision".into(),
            expected: Some(7),
            actual: Some(8)
        })
    );
    assert_eq!(counts.calls(), 0);
}

#[tokio::test]
async fn a_changed_count_is_refused() {
    let (before, after, mut counts) = case("remove-kind");
    let fixed = FixedCounts::new(counts.clone());
    let plan = dry_run(7, Some(&before), &after, &fixed).await.unwrap();
    // Someone made a folder between the dry run and the apply.
    counts.kinds_removed.get_mut("folder").unwrap().nodes = 15;
    fixed.set(counts);
    let got = apply(
        7,
        Some(&before),
        &after,
        Some(&Expect { revision: 7 }),
        Some(&plan.confirm),
        &fixed,
    )
    .await;
    assert_eq!(
        got,
        Err(Refused::Stale {
            precondition: "confirm.folder".into(),
            expected: Some(14),
            actual: Some(15)
        })
    );
}

#[tokio::test]
async fn an_unconfirmed_or_overconfirmed_plan_is_refused() {
    let (before, after, counts) = case("remove-kind");
    let counts = FixedCounts::new(counts);
    let expect = Expect { revision: 7 };
    let got = apply(7, Some(&before), &after, Some(&expect), None, &counts).await;
    assert_eq!(
        got,
        Err(Refused::Stale {
            precondition: "confirm.folder".into(),
            expected: None,
            actual: Some(14)
        })
    );
    let mut extra = confirm("folder", 14);
    extra.insert("project".into(), Confirmed { nodes: 3 });
    let got = apply(
        7,
        Some(&before),
        &after,
        Some(&expect),
        Some(&extra),
        &counts,
    )
    .await;
    assert_eq!(
        got,
        Err(Refused::Stale {
            precondition: "confirm.project".into(),
            expected: Some(3),
            actual: None
        })
    );
}

#[tokio::test]
async fn an_apply_needs_expect() {
    let (before, after, counts) = case("roles");
    let got = apply(
        7,
        Some(&before),
        &after,
        None,
        None,
        &FixedCounts::new(counts),
    )
    .await;
    assert!(matches!(got, Err(Refused::Invalid(e)) if e[0].path == "expect"));
}

#[tokio::test]
async fn a_blocked_plan_is_refused() {
    let (before, after, counts) = case("narrow-with-nodes");
    let got = apply(
        7,
        Some(&before),
        &after,
        Some(&Expect { revision: 7 }),
        None,
        &FixedCounts::new(counts),
    )
    .await;
    assert!(
        matches!(&got, Err(Refused::Invalid(e)) if e[0].path == "kinds.project.parent"),
        "{got:?}"
    );
}

#[tokio::test]
async fn identity_is_asked_only_when_the_plan_needs_counts() {
    // The first apply, as the seed makes it with identity not yet up.
    let after = load(&fixtures().join("first-apply/after.yml"));
    dry_run(0, None, &after, &NoCounts).await.unwrap();
    apply(
        0,
        None,
        &after,
        Some(&Expect { revision: 0 }),
        None,
        &NoCounts,
    )
    .await
    .unwrap();
    let (before, after, _) = case("roles");
    dry_run(7, Some(&before), &after, &NoCounts).await.unwrap();

    let (before, after, _) = case("remove-kind");
    assert!(matches!(
        dry_run(7, Some(&before), &after, &NoCounts).await,
        Err(Refused::CountsUnavailable(_))
    ));
}
