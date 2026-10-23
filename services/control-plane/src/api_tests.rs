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

use crate::counts::{CountsResponse, Effects, FixedCounts};
use crate::feed::Feed;
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
    let counts = Arc::new(FixedCounts::new(CountsResponse {
        kinds_removed: [(
            "folder".to_string(),
            Effects {
                nodes: 14,
                bindings: 22,
                service_accounts: 3,
                keys: 9,
            },
        )]
        .into(),
        parents_narrowed: Default::default(),
    }));
    let state = AppState {
        feed: Feed::spawn(store.clone(), 0),
        store,
        counts: counts.clone(),
        admin_token: TOKEN.into(),
        internal_token: None,
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

    // Auth.
    let resp = api
        .public
        .clone()
        .oneshot(Request::get("/config").body(Body::empty()).unwrap())
        .await
        .unwrap();
    assert_eq!(resp.status(), StatusCode::UNAUTHORIZED);

    // The first apply: a folder kind, projects under orgs or folders.
    let first = fixture("remove-kind", "before.yml");
    let (status, plan) = api
        .admin("POST", "/apply?dry_run=true", Some(first.clone()))
        .await;
    assert_eq!(status, StatusCode::OK, "{plan}");
    assert_eq!(plan["revision"], json!(0));
    assert_eq!(plan["confirm"], json!({}));

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

    // Stored as sent; served shaped for each reader.
    let (_, config) = api.admin("GET", "/config", None).await;
    assert_eq!(config["revision"], json!(1));
    assert_eq!(config["config"], first);
    let (status, gateway) = api.feed("/internal/gateway?after=0").await;
    assert_eq!(status, StatusCode::OK);
    assert_eq!(gateway["revision"], json!(1));
    assert_eq!(gateway["cors"], json!({ "allowed_origins": [] }));
    assert_eq!(
        gateway["services"]["e10s"]["routes"][0]["methods"],
        json!(["GET"])
    );
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

    // Removing folder is destructive.
    let next = fixture("remove-kind", "after.yml");
    let (_, plan) = api
        .admin("POST", "/apply?dry_run=true", Some(next.clone()))
        .await;
    assert_eq!(plan["revision"], json!(1));
    assert_eq!(plan["confirm"], json!({ "folder": { "nodes": 14 } }));

    // A stale expect.
    let (status, err) = api
        .admin(
            "POST",
            "/apply",
            Some(with(
                next.clone(),
                json!({ "expect": { "revision": 0 }, "confirm": plan["confirm"] }),
            )),
        )
        .await;
    assert_eq!(status, StatusCode::CONFLICT);
    assert_eq!(err["error"]["code"], json!("PLAN_STALE"));
    assert_eq!(
        err["error"]["details"],
        json!({ "precondition": "revision", "expected": 0, "actual": 1 })
    );

    // A folder was made since the dry run: the confirmed count is stale.
    counts.set(CountsResponse {
        kinds_removed: [(
            "folder".to_string(),
            Effects {
                nodes: 15,
                ..Default::default()
            },
        )]
        .into(),
        parents_narrowed: Default::default(),
    });
    let (status, err) = api
        .admin(
            "POST",
            "/apply",
            Some(with(
                next.clone(),
                json!({ "expect": { "revision": 1 }, "confirm": plan["confirm"] }),
            )),
        )
        .await;
    assert_eq!(status, StatusCode::CONFLICT);
    assert_eq!(
        err["error"]["details"],
        json!({ "precondition": "confirm.folder", "expected": 14, "actual": 15 })
    );
    assert_eq!(
        api.admin("GET", "/config", None).await.1["revision"],
        json!(1)
    );

    // Confirmed at the new count, it applies.
    let (status, applied) = api
        .admin(
            "POST",
            "/apply",
            Some(with(
                next.clone(),
                json!({ "expect": { "revision": 1 }, "confirm": { "folder": { "nodes": 15 } } }),
            )),
        )
        .await;
    assert_eq!(status, StatusCode::OK, "{applied}");
    assert_eq!(applied["revision"], json!(2));
    assert_eq!(
        applied["summary"],
        json!({ "add": 0, "change": 2, "remove": 2, "blocked": 0 })
    );

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

    // Validation names field paths; ownership is a 409.
    let mut bad = next.clone();
    bad["services"]["e10s"]["routes"][0]["scope"] = json!("team");
    bad["expect"] = json!({ "revision": 2 });
    let (status, err) = api.admin("POST", "/apply", Some(bad)).await;
    assert_eq!(status, StatusCode::UNPROCESSABLE_ENTITY);
    assert_eq!(
        err["error"]["details"]["fields"][0]["path"],
        json!("services.e10s.routes[0].scope")
    );
    let mut clash = next.clone();
    clash["services"]["other"] = json!({ "url": "http://o:1", "routes": [{ "path": "/health", "scope": "public", "methods": ["GET"] }] });
    let (status, err) = api.admin("POST", "/apply?dry_run=true", Some(clash)).await;
    assert_eq!(status, StatusCode::CONFLICT);
    assert_eq!(err["error"]["code"], json!("ROUTE_CONFLICT"));

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

    // The catalog for kinds not yet applied.
    let (status, catalog) = api
        .admin(
            "POST",
            "/catalog",
            Some(json!({ "version": 2, "kinds": first["kinds"] })),
        )
        .await;
    assert_eq!(status, StatusCode::OK);
    assert!(
        catalog["services"]["identity"]["routes"]
            .as_array()
            .unwrap()
            .len()
            > 20
    );
    let (status, _) = api
        .admin(
            "POST",
            "/catalog",
            Some(json!({ "version": 2, "kinds": { "org": {} } })),
        )
        .await;
    assert_eq!(status, StatusCode::UNPROCESSABLE_ENTITY);

    let (status, schema) = api.admin("GET", "/schema", None).await;
    assert_eq!(status, StatusCode::OK);
    assert_eq!(
        schema["config.json"]["$id"],
        json!("https://plat5.com/schema/v2/config.json")
    );
}
