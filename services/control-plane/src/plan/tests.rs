//! Fixture configs in `fixtures/plans/<case>/`:
//!
//! - `before.yml`: the applied config; absent before the first apply
//! - `after.yml`: the config planned
//! - `counts.json`: what identity answers, if the plan asks
//! - `plan.json`: the plan, and `gateway-feed.json`: what the gateway is served
//!   for `after.yml`; or `refused.json`: the rule errors `after.yml` is refused
//!   with
//!
//! A dry run answers the plan with the feed's `services` beside it; `plan.json`
//! holds the plan alone, and `gateway-feed.json` the services.
//!
//! `plan.json`, `gateway-feed.json` and `refused.json` are derived:
//! `UPDATE_FIXTURES=1 cargo test plan` rewrites them.

use std::path::{Path, PathBuf};

use serde_json::{json, Value};

use super::*;
use crate::apply::{self, Failure};
use crate::counts::{CountsResponse, FixedCounts, KindCounts, NoCounts};
use crate::fixtures;

fn plans() -> PathBuf {
    fixtures::dir().join("plans")
}

fn load(path: &Path) -> Config {
    match apply::check(&fixtures::yaml(path)) {
        Ok(c) => c,
        Err(f) => panic!("{}: {f:?}", path.display()),
    }
}

fn read_counts(dir: &Path) -> CountsResponse {
    std::fs::read_to_string(dir.join("counts.json"))
        .map(|t| serde_json::from_str(&t).unwrap())
        .unwrap_or_default()
}

#[tokio::test]
async fn fixture_configs_plan() {
    let mut cases: Vec<PathBuf> = std::fs::read_dir(plans())
        .unwrap()
        .map(|e| e.unwrap().path())
        .filter(|p| p.is_dir())
        .collect();
    cases.sort();
    assert!(cases.len() >= 15, "fixtures missing: {cases:?}");
    for case in cases {
        let name = case.file_name().unwrap().to_string_lossy().to_string();
        let (plan_file, feed_file, refused_file) = (
            case.join("plan.json"),
            case.join("gateway-feed.json"),
            case.join("refused.json"),
        );
        let after = match apply::check(&fixtures::yaml(&case.join("after.yml"))) {
            Ok(c) => c,
            Err(Failure::Invalid(errs)) => {
                fixtures::check_json(&refused_file, &json!(errs));
                if fixtures::updating() {
                    let _ = std::fs::remove_file(&plan_file);
                    let _ = std::fs::remove_file(&feed_file);
                }
                assert!(!plan_file.exists(), "{name}: refused, but has a plan.json");
                continue;
            }
            Err(f) => panic!("{name}: {f:?}"),
        };
        if fixtures::updating() {
            let _ = std::fs::remove_file(&refused_file);
        }
        assert!(
            !refused_file.exists(),
            "{name}: accepted, but has a refused.json"
        );
        let before = case.join("before.yml");
        let before = before.exists().then(|| load(&before));
        let revision = if before.is_some() { 7 } else { 0 };
        let counts = FixedCounts::new(read_counts(&case));
        let plan = dry_run(revision, before.as_ref(), &after, &counts)
            .await
            .unwrap_or_else(|e| panic!("{name}: {e:?}"));
        fixtures::check_json(&plan_file, &serde_json::to_value(&plan).unwrap());
        fixtures::check_json(&feed_file, &apply::gateway_body(revision + 1, &after));
    }
}

fn case(name: &str) -> (Config, Config, CountsResponse) {
    let dir = plans().join(name);
    (
        load(&dir.join("before.yml")),
        load(&dir.join("after.yml")),
        read_counts(&dir),
    )
}

#[tokio::test]
async fn an_apply_recomputes_the_plan() {
    let (before, after, counts) = case("remove-kind");
    let counts = FixedCounts::new(counts);
    let plan = dry_run(7, Some(&before), &after, &counts).await.unwrap();
    let applied = apply(
        7,
        Some(&before),
        &after,
        Some(&Expect { revision: 7 }),
        &counts,
    )
    .await
    .unwrap();
    assert_eq!(applied, plan);
    assert_eq!(counts.calls(), 2);
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
async fn an_apply_needs_expect() {
    let (before, after, counts) = case("roles");
    let got = apply(7, Some(&before), &after, None, &FixedCounts::new(counts)).await;
    assert!(matches!(got, Err(Refused::Invalid(e)) if e[0].path == "expect"));
}

#[tokio::test]
async fn a_blocked_plan_is_refused() {
    for (name, path) in [
        ("remove-kind-blocked", "kinds.folder"),
        ("parent-change-blocked", "kinds.environment.parent"),
    ] {
        let (before, after, counts) = case(name);
        let got = apply(
            7,
            Some(&before),
            &after,
            Some(&Expect { revision: 7 }),
            &FixedCounts::new(counts),
        )
        .await;
        assert!(
            matches!(&got, Err(Refused::Invalid(e)) if e.len() == 1 && e[0].path == path),
            "{name}: {got:?}"
        );
    }
}

#[tokio::test]
async fn a_node_made_since_the_dry_run_blocks_the_apply() {
    let (before, after, counts) = case("remove-kind");
    let fixed = FixedCounts::new(counts.clone());
    let plan = dry_run(7, Some(&before), &after, &fixed).await.unwrap();
    assert_eq!(plan.summary.blocked, 0);
    fixed.set(CountsResponse {
        kinds: [(
            "folder".to_string(),
            KindCounts {
                nodes: 1,
                sample: vec!["01jb8z3m5q0000000000000009".into()],
            },
        )]
        .into(),
    });
    let got = apply(
        7,
        Some(&before),
        &after,
        Some(&Expect { revision: 7 }),
        &fixed,
    )
    .await;
    assert!(
        matches!(&got, Err(Refused::Invalid(e)) if e[0].path == "kinds.folder"),
        "{got:?}"
    );
}

#[tokio::test]
async fn identity_is_asked_only_when_the_plan_needs_counts() {
    // The first apply, with identity not yet up.
    let after = load(&plans().join("first-apply/after.yml"));
    dry_run(0, None, &after, &NoCounts).await.unwrap();
    apply(0, None, &after, Some(&Expect { revision: 0 }), &NoCounts)
        .await
        .unwrap();
    for name in [
        "roles",
        "services",
        "add-kind-at-bottom",
        "identity-overrides",
    ] {
        let (before, after, _) = case(name);
        dry_run(7, Some(&before), &after, &NoCounts)
            .await
            .unwrap_or_else(|e| panic!("{name}: {e:?}"));
    }
    for name in ["remove-kind", "parent-change-empty"] {
        let (before, after, _) = case(name);
        assert!(
            matches!(
                dry_run(7, Some(&before), &after, &NoCounts).await,
                Err(Refused::CountsUnavailable(_))
            ),
            "{name}"
        );
    }
}

#[test]
fn diff_keeps_only_what_differs() {
    let (b, a) = diff(
        &json!({ "parent": "organization", "path": "/x/{x}" }),
        &json!({ "parent": "project", "path": "/x/{x}", "list_permission": "x:read" }),
    );
    assert_eq!(b, Some(json!({ "parent": "organization" })));
    assert_eq!(
        a,
        Some(json!({ "parent": "project", "list_permission": "x:read" }))
    );
    let (b, a) = diff(&json!("30d"), &json!("7d"));
    assert_eq!((b, a), (Some(json!("30d")), Some(json!("7d"))));
    let (b, _) = diff(&json!({}), &json!({ "a": 1 }));
    assert_eq!(b, None::<Value>);
}
