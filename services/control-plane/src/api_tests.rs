//! The admin API and feeds against a real Postgres, end to end through the
//! routers. Set `TEST_DATABASE_URL` to run it; it drops and recreates the
//! `control_plane` schema there. Without it the test is skipped.

use std::path::Path;
use std::sync::Arc;

use axum::body::Body;
use axum::http::{Request, StatusCode};
use axum::Router;
use serde_json::{json, Value};
use tower::ServiceExt;

use crate::counts::{CountsResponse, FixedCounts, KindCounts};
use crate::feed::Feed;
use crate::fixtures;
use crate::store::Store;
use crate::AppState;

const TOKEN: &str = "test-admin-token";

fn fixture(case: &str, file: &str) -> Value {
    let path = Path::new(env!("CARGO_MANIFEST_DIR"))
        .join("fixtures/plans")
        .join(case)
        .join(file);
    let yaml: serde_yaml::Value =
        serde_yaml::from_str(&std::fs::read_to_string(path).unwrap()).unwrap();
    serde_json::to_value(yaml).unwrap()
}

struct Api {
    public: Router,
    internal: Router,
}

impl Api {
    async fn call(
        &self,
        router: &Router,
        method: &str,
        uri: &str,
        body: Option<Value>,
    ) -> (StatusCode, Value) {
        let mut req = Request::builder()
            .method(method)
            .uri(uri)
            .header("authorization", format!("Bearer {TOKEN}"));
        let body = match body {
            Some(b) => {
                req = req.header("content-type", "application/json");
                Body::from(b.to_string())
            }
            None => Body::empty(),
        };
        let resp = router
            .clone()
            .oneshot(req.body(body).unwrap())
            .await
            .unwrap();
        let status = resp.status();
        let bytes = axum::body::to_bytes(resp.into_body(), 1 << 22)
            .await
            .unwrap();
        let value = serde_json::from_slice(&bytes).unwrap_or(Value::Null);
        (status, value)
    }

    async fn admin(&self, method: &str, uri: &str, body: Option<Value>) -> (StatusCode, Value) {
        self.call(&self.public, method, uri, body).await
    }

    async fn feed(&self, uri: &str) -> (StatusCode, Value) {
        self.call(&self.internal, "GET", uri, None).await
    }
}

fn with(mut config: Value, extra: Value) -> Value {
    for (k, v) in extra.as_object().unwrap() {
        config[k] = v.clone();
    }
    config
}

fn folder_counts(nodes: u64) -> CountsResponse {
    CountsResponse {
        kinds: [(
            "folder".to_string(),
            KindCounts {
                nodes,
                sample: (0..nodes)
                    .map(|i| format!("01jb8z3m5q000000000000000{i}"))
                    .collect(),
            },
        )]
        .into(),
    }
}

#[tokio::test]
async fn plan_and_apply_against_postgres() {
    let Ok(url) = std::env::var("TEST_DATABASE_URL") else {
        eprintln!("skipped: set TEST_DATABASE_URL to run against Postgres");
        return;
    };
    {
        let pool = sqlx::PgPool::connect(&url).await.unwrap();
        sqlx::query("DROP SCHEMA IF EXISTS control_plane CASCADE")
            .execute(&pool)
            .await
            .unwrap();
    }
    let store = Store::connect(&url).await.unwrap();
    let counts = Arc::new(FixedCounts::new(folder_counts(2)));
    let state = AppState {
        feed: Feed::spawn(store.clone(), 0),
        store,
        counts: counts.clone(),
        admin_token: TOKEN.into(),
    };
    let api = Api {
        public: crate::api::public_router(state.clone()),
        internal: crate::api::internal_router(state),
    };

    // Before the first apply, nothing to read.
    assert_eq!(
        api.admin("GET", "/config", None).await.0,
        StatusCode::NOT_FOUND
    );
    assert_eq!(api.feed("/internal/gateway").await.0, StatusCode::NOT_FOUND);
    assert_eq!(
        api.feed("/internal/identity").await.0,
        StatusCode::NOT_FOUND
    );

    // Auth: the admin token, compared whole; the schema is open.
    for auth in [None, Some("Bearer wrong"), Some("Bearer test-admin-tok")] {
        let mut req = Request::get("/config");
        if let Some(a) = auth {
            req = req.header("authorization", a);
        }
        let resp = api
            .public
            .clone()
            .oneshot(req.body(Body::empty()).unwrap())
            .await
            .unwrap();
        assert_eq!(resp.status(), StatusCode::UNAUTHORIZED, "{auth:?}");
    }
    let resp = api
        .public
        .clone()
        .oneshot(Request::get("/schema").body(Body::empty()).unwrap())
        .await
        .unwrap();
    assert_eq!(resp.status(), StatusCode::OK);
    let bytes = axum::body::to_bytes(resp.into_body(), 1 << 22)
        .await
        .unwrap();
    let schema: Value = serde_json::from_slice(&bytes).unwrap();
    assert_eq!(
        schema["config.json"]["$id"],
        json!("https://plat5.com/schema/config.json")
    );
    assert_eq!(
        api.admin("POST", "/catalog", Some(json!({}))).await.0,
        StatusCode::NOT_FOUND
    );

    // The first apply: organizations, projects and folders.
    let first = fixture("remove-kind", "before.yml");
    let (status, plan) = api
        .admin("POST", "/apply?dry_run=true", Some(first.clone()))
        .await;
    assert_eq!(status, StatusCode::OK, "{plan}");
    assert_eq!(plan["revision"], json!(0));
    assert_eq!(counts.calls(), 0);

    let (status, err) = api.admin("POST", "/apply", Some(first.clone())).await;
    assert_eq!(status, StatusCode::UNPROCESSABLE_ENTITY);
    assert_eq!(
        err["error"]["details"]["fields"][0]["path"],
        json!("expect")
    );

    let (status, applied) = api
        .admin(
            "POST",
            "/apply",
            Some(with(first.clone(), json!({ "expect": { "revision": 0 } }))),
        )
        .await;
    assert_eq!(status, StatusCode::OK, "{applied}");
    assert_eq!(applied["revision"], json!(1));
    assert_eq!(applied["summary"], plan["summary"]);
    assert!(applied.get("services").is_none());

    // Stored as sent; served shaped for each reader.
    let (_, config) = api.admin("GET", "/config", None).await;
    assert_eq!(config["revision"], json!(1));
    assert_eq!(config["config"], first);
    let (status, gateway) = api.feed("/internal/gateway?after=0").await;
    assert_eq!(status, StatusCode::OK);
    assert_eq!(gateway["revision"], json!(1));
    // The dry run's services are what the feed serves once it's applied.
    assert_eq!(plan["services"], gateway["services"]);
    assert_eq!(
        gateway["cors"],
        json!({ "allowed_origins": [], "allowed_headers": [] })
    );
    let routes = gateway["services"]["e10s"]["routes"].as_array().unwrap();
    assert!(routes.iter().any(|r| r["path"]
        == json!("/orgs/{organization}/folders/{folder}/plans")
        && r["upstream"] == json!("/folders/{folder.id}/plans")));
    let (_, identity) = api.feed("/internal/identity").await;
    assert_eq!(identity["deletion_grace"], json!("30d"));
    assert_eq!(identity["creator_role"], json!("admin"));

    // The same config again: nothing to write.
    let (status, again) = api
        .admin(
            "POST",
            "/apply",
            Some(with(first.clone(), json!({ "expect": { "revision": 1 } }))),
        )
        .await;
    assert_eq!(status, StatusCode::OK);
    assert_eq!(again["revision"], json!(1));

    // Removing folder while it has nodes is blocked.
    let next = fixture("remove-kind", "after.yml");
    let (_, plan) = api
        .admin("POST", "/apply?dry_run=true", Some(next.clone()))
        .await;
    assert_eq!(plan["revision"], json!(1));
    assert_eq!(plan["summary"]["blocked"], json!(1));
    let folder = plan["changes"]
        .as_array()
        .unwrap()
        .iter()
        .find(|c| c["what"] == json!("kind"))
        .unwrap();
    assert_eq!(
        folder["blocked"],
        json!({ "reason": "kind_not_empty", "in_the_way": { "nodes": 2, "sample": [
            "01jb8z3m5q0000000000000000", "01jb8z3m5q0000000000000001"
        ] } })
    );
    let (status, err) = api
        .admin(
            "POST",
            "/apply",
            Some(with(next.clone(), json!({ "expect": { "revision": 1 } }))),
        )
        .await;
    assert_eq!(status, StatusCode::UNPROCESSABLE_ENTITY);
    assert_eq!(
        err["error"]["details"]["fields"][0]["path"],
        json!("kinds.folder")
    );

    // A stale expect.
    let (status, err) = api
        .admin(
            "POST",
            "/apply",
            Some(with(next.clone(), json!({ "expect": { "revision": 0 } }))),
        )
        .await;
    assert_eq!(status, StatusCode::CONFLICT);
    assert_eq!(err["error"]["code"], json!("PLAN_STALE"));
    assert_eq!(
        err["error"]["details"],
        json!({ "precondition": "revision", "expected": 0, "actual": 1 })
    );

    // Its nodes deleted but not purged still block it.
    counts.set(folder_counts(1));
    let (status, err) = api
        .admin(
            "POST",
            "/apply",
            Some(with(next.clone(), json!({ "expect": { "revision": 1 } }))),
        )
        .await;
    assert_eq!(status, StatusCode::UNPROCESSABLE_ENTITY);
    assert_eq!(
        err["error"]["details"]["fields"][0],
        json!({
            "path": "kinds.folder",
            "message": "blocked (kind_not_empty): folder has 1 nodes, live or deleted; delete them first and wait for the purge, such as 01jb8z3m5q0000000000000000"
        })
    );

    // Once its nodes are purged, folder goes.
    counts.set(folder_counts(0));
    let (status, applied) = api
        .admin(
            "POST",
            "/apply",
            Some(with(next.clone(), json!({ "expect": { "revision": 1 } }))),
        )
        .await;
    assert_eq!(status, StatusCode::OK, "{applied}");
    assert_eq!(applied["revision"], json!(2));
    assert_eq!(
        applied["summary"],
        json!({ "add": 0, "change": 2, "remove": 1, "blocked": 0 })
    );
    let (_, gateway) = api.feed("/internal/gateway?after=1").await;
    assert_eq!(gateway["revision"], json!(2));
    assert_eq!(plan["services"], gateway["services"]);

    let (_, revisions) = api.admin("GET", "/revisions", None).await;
    let revs: Vec<&Value> = revisions["revisions"]
        .as_array()
        .unwrap()
        .iter()
        .map(|r| &r["revision"])
        .collect();
    assert_eq!(revs, [&json!(2), &json!(1)]);
    let (_, one) = api.admin("GET", "/revisions/1", None).await;
    assert_eq!(one["config"], first);
    assert_eq!(
        api.admin("GET", "/revisions/9", None).await.0,
        StatusCode::NOT_FOUND
    );

    // Validation names field paths, two routes of one shape included.
    let mut bad = next.clone();
    bad["services"]["e10s"]["scopes"]["team"] = bad["services"]["e10s"]["scopes"]["public"].clone();
    bad["expect"] = json!({ "revision": 2 });
    let (status, err) = api.admin("POST", "/apply", Some(bad)).await;
    assert_eq!(status, StatusCode::UNPROCESSABLE_ENTITY);
    assert_eq!(
        err["error"]["details"]["fields"][0]["path"],
        json!("services.e10s.scopes.team")
    );
    let mut clash = next.clone();
    clash["services"]["other"] = json!({ "url": "http://o:1", "scopes": { "public": { "routes": [{ "path": "/health", "methods": ["GET"] }] } } });
    let (status, err) = api.admin("POST", "/apply?dry_run=true", Some(clash)).await;
    assert_eq!(status, StatusCode::UNPROCESSABLE_ENTITY);
    assert_eq!(
        err["error"]["details"]["fields"][0]["path"],
        json!("services.other.scopes.public.routes[0].path")
    );

    // YAML bodies.
    let yaml = std::fs::read_to_string(
        Path::new(env!("CARGO_MANIFEST_DIR")).join("fixtures/plans/remove-kind/after.yml"),
    )
    .unwrap();
    let resp = api
        .public
        .clone()
        .oneshot(
            Request::post("/apply?dry_run=true")
                .header("authorization", format!("Bearer {TOKEN}"))
                .header("content-type", "application/yaml")
                .body(Body::from(yaml))
                .unwrap(),
        )
        .await
        .unwrap();
    assert_eq!(resp.status(), StatusCode::OK);

    // Gateway replicas report on their long poll; the status lists them.
    let (_, status) = api.admin("GET", "/status", None).await;
    assert_eq!(status, json!({ "revision": 2, "gateway": [] }));
    let (code, _) = api
        .feed("/internal/gateway?replica=gw-2&loaded=1&refused=2&reason=route%20%2Fx%3A%20bad")
        .await;
    assert_eq!(code, StatusCode::OK);
    api.feed("/internal/gateway?replica=gw-1&loaded=1").await;
    api.feed("/internal/gateway?replica=gw-1&loaded=2").await;
    let (_, status) = api.admin("GET", "/status", None).await;
    assert_eq!(status["revision"], json!(2));
    let gateway = status["gateway"].as_array().unwrap();
    assert_eq!(gateway.len(), 2);
    assert_eq!(gateway[0]["replica"], json!("gw-1"));
    assert_eq!(gateway[0]["loaded"], json!(2));
    assert_eq!(gateway[0]["refused"], Value::Null);
    assert_eq!(
        gateway[1]["refused"],
        json!({ "revision": 2, "reason": "route /x: bad" })
    );
    assert!(gateway[1]["seen_at"].is_string());

    // Identity's and audit's routes too: expanded from their resources.
    let catalog = fixtures::yaml(&fixtures::dir().join("catalog/plat5.yml"));
    let (status, plan) = api
        .admin("POST", "/apply?dry_run=true", Some(catalog.clone()))
        .await;
    assert_eq!(status, StatusCode::OK, "{plan}");
    let routes = |s: &str| plan["services"][s]["routes"].as_array().unwrap().len();
    assert!(routes("identity") > 0 && routes("audit") > 0);
    let (status, applied) = api
        .admin(
            "POST",
            "/apply",
            Some(with(catalog, json!({ "expect": { "revision": 2 } }))),
        )
        .await;
    assert_eq!(status, StatusCode::OK, "{applied}");
    let (_, gateway) = api.feed("/internal/gateway?after=2").await;
    assert_eq!(gateway["revision"], json!(3));
    assert_eq!(plan["services"], gateway["services"]);
}
