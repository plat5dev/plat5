//! The pipeline against the catalog fixture
//! (services/control-plane/fixtures/catalog/identity-feed.json): every generated
//! route, and every row of the admit table (docs/reference/gateway.md#admit).

use std::collections::HashMap;
use std::sync::atomic::Ordering;
use std::sync::{Arc, Mutex};
use std::time::Duration;

use serde_json::{json, Value};

use super::*;
use crate::auth::admit::tests::{admitted, invalid, not_found, FakeAdmit};
use crate::auth::admit::{AdmitAnswer, NodeRef};
use crate::control_plane::{build, Snapshot};
use crate::rate_limit::{RateLimitError, RateLimitInfo};

const IP: &str = "203.0.113.7";

fn key() -> String {
    format!("plat5-k-2-{}", "A".repeat(43))
}

/// The fixture's identity feed, as the gateway feed for the same config.
fn fixture() -> Value {
    let path = concat!(
        env!("CARGO_MANIFEST_DIR"),
        "/../control-plane/fixtures/catalog/identity-feed.json"
    );
    serde_json::from_str(&std::fs::read_to_string(path).expect("catalog fixture")).unwrap()
}

fn snapshot_with(extra_services: Value) -> Snapshot {
    let f = fixture();
    let mut services = f["services"].clone();
    for (name, svc) in extra_services.as_object().unwrap() {
        services[name] = svc.clone();
    }
    build(json!({
        "revision": f["revision"],
        "kinds": f["kinds"],
        "services": services,
        "cors": { "allowed_origins": [] }
    }))
    .expect("the catalog fixture builds")
}

fn catalog() -> Snapshot {
    snapshot_with(json!({}))
}

/// Fixed-window counters in memory, recording each bucket counted.
#[derive(Default)]
struct MemCounter {
    counts: Mutex<HashMap<String, u64>>,
    order: Mutex<Vec<String>>,
}

impl MemCounter {
    fn count(&self, bucket: &str) -> u64 {
        self.counts
            .lock()
            .unwrap()
            .get(bucket)
            .copied()
            .unwrap_or(0)
    }
    fn buckets(&self) -> Vec<String> {
        self.order.lock().unwrap().clone()
    }
}

impl Counter for Arc<MemCounter> {
    async fn allow(
        &self,
        bucket: &str,
        limit: u64,
        window_seconds: u64,
    ) -> Result<RateLimitInfo, RateLimitError> {
        if limit == 0 {
            return Ok(RateLimitInfo {
                limit: 0,
                remaining: 0,
                reset_epoch: 1,
            });
        }
        self.order.lock().unwrap().push(bucket.to_string());
        let mut counts = self.counts.lock().unwrap();
        let n = counts.entry(bucket.to_string()).or_default();
        *n += 1;
        let info = RateLimitInfo {
            limit,
            remaining: limit.saturating_sub(*n),
            reset_epoch: window_seconds,
        };
        if *n > limit {
            Err(RateLimitError::Exceeded {
                retry_after: window_seconds,
                info,
            })
        } else {
            Ok(info)
        }
    }
}

struct Harness {
    pipeline: Pipeline<Arc<FakeAdmit>, Arc<MemCounter>>,
    admit: Arc<FakeAdmit>,
    counter: Arc<MemCounter>,
}

fn harness(answer: Result<AdmitAnswer, String>) -> Harness {
    harness_with(
        answer,
        Limit {
            requests: 60,
            window_seconds: 60,
        },
        Limit {
            requests: 60,
            window_seconds: 60,
        },
    )
}

fn harness_with(
    answer: Result<AdmitAnswer, String>,
    fallback: Limit,
    auth_failure: Limit,
) -> Harness {
    let admit = Arc::new(FakeAdmit::new(answer));
    let counter = Arc::new(MemCounter::default());
    Harness {
        pipeline: Pipeline::new(
            AdmitCache::new(admit.clone(), Duration::from_secs(300)),
            counter.clone(),
            fallback,
            auth_failure,
        ),
        admit,
        counter,
    }
}

impl Harness {
    fn set_answer(&self, answer: AdmitAnswer) {
        *self.admit.answer.lock().unwrap() = Ok(answer);
    }

    async fn run(
        &self,
        route: &Route,
        params: &HashMap<String, String>,
        presented: Result<Presented, AuthError>,
    ) -> (Result<Passed, Rejection>, RequestCredential) {
        let mut cred = RequestCredential::default();
        // GET when the route has it, else the first in order: methods is a set.
        let method = if route.methods.contains("GET") {
            "GET".to_string()
        } else {
            route.methods.iter().min().unwrap().clone()
        };
        let r = self
            .pipeline
            .admit_and_limit(route, params, &method, IP, presented, &mut cred)
            .await;
        (r, cred)
    }

    /// The whole decision: admit and limit, the label check, and the fill.
    async fn decide(
        &self,
        route: &Route,
        params: &HashMap<String, String>,
        presented: Result<Presented, AuthError>,
    ) -> Result<(Admitted, Forward), Rejection> {
        let passed = self.run(route, params, presented).await.0?;
        authorize(route, &passed.admitted)?;
        let fwd = forward(route, params, &passed.admitted)?;
        Ok((passed.admitted, fwd))
    }
}

/// A concrete request path for a route, and its params: a slug per chain kind,
/// a value per other param.
fn concrete(route: &Route) -> (String, HashMap<String, String>) {
    let mut params = HashMap::new();
    let path = route
        .path
        .split('/')
        .map(
            |seg| match seg.strip_prefix('{').and_then(|s| s.strip_suffix('}')) {
                Some(name) => {
                    let value = if route.is_chain_param(name) {
                        format!("{}-slug", name.replace('_', "-"))
                    } else {
                        format!("v-{name}")
                    };
                    params.insert(name.to_string(), value.clone());
                    value
                }
                None => seg.to_string(),
            },
        )
        .collect::<Vec<_>>()
        .join("/");
    (path, params)
}

fn node_id(kind: &str) -> String {
    format!("n_{kind}")
}

fn chain_nodes(route: &Route) -> Vec<(String, String)> {
    route
        .chain
        .iter()
        .map(|k| (k.clone(), node_id(k)))
        .collect()
}

fn answer_for(route: &Route, labels: &[&str]) -> AdmitAnswer {
    let nodes = chain_nodes(route);
    let pairs: Vec<(&str, &str)> = nodes
        .iter()
        .map(|(k, i)| (k.as_str(), i.as_str()))
        .collect();
    admitted(&pairs, labels)
}

fn find<'a>(
    snapshot: &'a Snapshot,
    method: &str,
    path: &str,
) -> (&'a Route, HashMap<String, String>) {
    snapshot
        .routes
        .find_route(path, method)
        .unwrap_or_else(|| panic!("no route for {method} {path}"))
}

fn header<'a>(fwd: &'a Forward, name: &str) -> Option<&'a str> {
    fwd.headers
        .iter()
        .find(|(n, _)| n.eq_ignore_ascii_case(name))
        .map(|(_, v)| v.as_str())
}

#[tokio::test]
async fn every_catalog_route_matches_admits_and_fills_its_upstream() {
    let snapshot = catalog();
    let mut seen = 0;
    let mut restore_routes = 0;
    for route in snapshot.routes.routes() {
        for method in &route.methods {
            seen += 1;
            let (path, params) = concrete(route);
            let (matched, matched_params) = find(&snapshot, method, &path);
            assert_eq!(
                (matched.service.as_str(), matched.path.as_str()),
                (route.service.as_str(), route.path.as_str()),
                "{method} {path} matched another route"
            );
            assert_eq!(matched_params, params);

            let h = harness(Ok(answer_for(route, &["*"])));
            let (admitted, fwd) = h
                .decide(matched, &params, Ok(Presented::Key(key())))
                .await
                .unwrap_or_else(|r| panic!("{method} {} refused: {r:?}", route.path));

            // Admit got the scope, the chain as written, and resolve_deleted.
            let req = h.admit.last.lock().unwrap().clone().unwrap();
            assert_eq!(req.scope, route.scope.name());
            let refs: Vec<(String, String)> = req
                .chain
                .iter()
                .map(|c| (c.kind.clone(), c.reference.clone()))
                .collect();
            let want: Vec<(String, String)> = route
                .chain
                .iter()
                .map(|k| (k.clone(), params[k].clone()))
                .collect();
            assert_eq!(refs, want, "{}", route.path);
            assert_eq!(req.resolve_deleted, route.resolve_deleted);
            if route.path.ends_with("/restore") {
                restore_routes += 1;
                assert!(
                    req.resolve_deleted,
                    "{} must resolve a deleted node",
                    route.path
                );
            }

            // The upstream is the template with ids: no slug ever reaches a service.
            let mut want_path = route
                .upstream
                .clone()
                .expect("generated routes have upstreams");
            for kind in &route.chain {
                want_path = want_path.replace(&format!("{{subject.{kind}_id}}"), &node_id(kind));
            }
            want_path = want_path.replace("{subject.member_id}", "m_1");
            for (name, value) in &params {
                want_path = want_path.replace(&format!("{{path.{name}}}"), value);
            }
            assert_eq!(
                fwd.path.as_deref(),
                Some(want_path.as_str()),
                "{}",
                route.path
            );
            for kind in &route.chain {
                assert!(
                    !fwd.path.as_deref().unwrap().contains(&params[kind]),
                    "{}: slug in upstream",
                    route.path
                );
            }

            // Subject headers: one per chain node, the member and the principal.
            let Admitted::Caller(caller) = admitted else {
                panic!("{} admitted as public", route.path)
            };
            assert_eq!(caller.nodes.len(), route.chain.len());
            for kind in &route.chain {
                assert_eq!(
                    header(&fwd, &crate::kinds::id_header(kind)),
                    Some(node_id(kind).as_str())
                );
            }
            assert_eq!(header(&fwd, MEMBER_ID_HEADER), Some("m_1"));
            assert_eq!(header(&fwd, PRINCIPAL_HEADER), Some("person"));
            assert_eq!(fwd.headers.len(), route.chain.len() + 2, "{}", route.path);

            match &route.scope {
                Scope::SignIn(kind) => {
                    assert_eq!(kind, "directory");
                    assert!(route.chain.is_empty());
                }
                Scope::Kind(kind) => assert_eq!(route.chain.last(), Some(kind)),
                Scope::Public => panic!("the catalog has no public routes"),
            }
        }
    }
    assert!(seen >= 90, "only {seen} route methods in the catalog");
    assert!(restore_routes >= 3, "a restore route per non-sign-in kind");
}

#[tokio::test]
async fn generated_labels_gate_their_routes() {
    let snapshot = catalog();
    for route in snapshot.routes.routes() {
        if route.required_labels.is_empty() {
            continue;
        }
        let (_, params) = concrete(route);
        for label in &route.required_labels {
            let h = harness(Ok(answer_for(route, &[label.as_str()])));
            assert!(h
                .decide(route, &params, Ok(Presented::Key(key())))
                .await
                .is_ok());
        }
        let h = harness(Ok(answer_for(route, &["some:other-label"])));
        assert_eq!(
            h.decide(route, &params, Ok(Presented::Key(key())))
                .await
                .err(),
            Some(Rejection::LabelDenied),
            "{}",
            route.path
        );
    }
}

/// `GET /orgs/{organization}/projects/{project}`: `project:read`.
fn project_read(snapshot: &Snapshot) -> (Route, HashMap<String, String>) {
    let (r, p) = find(snapshot, "GET", "/orgs/acme/projects/web");
    assert_eq!(r.required_labels, vec!["project:read"]);
    (r.clone(), p)
}

fn jwt() -> Presented {
    Presented::Jwt {
        token: "header.payload.sig".into(),
        issuer: "https://auth".into(),
        subject: "sub-1".into(),
        name: Some("Dana".into()),
        kid: Some("kid-1".into()),
    }
}

const IP_BUCKET: &str = "ip:203.0.113.7";

#[tokio::test]
async fn admit_table() {
    let snapshot = catalog();
    let (route, params) = project_read(&snapshot);
    let route_bucket = "GET /orgs/{organization}/projects/{project} n_project";
    let ok = || answer_for(&route, &["project:read"]);

    // valid: false → 401, counted on the failed-auth limiter, before the rate limit.
    let h = harness(Ok(invalid()));
    let (r, cred) = h.run(&route, &params, Ok(Presented::Key(key()))).await;
    assert_eq!(
        r.err(),
        Some(Rejection::Unauthorized(AuthError::InvalidKey))
    );
    assert_eq!(cred.key_prefix.as_deref(), Some("plat5-k-2-AAAA"));
    assert_eq!(h.counter.buckets(), vec![IP_BUCKET]);

    // A JWT for a suspended person is the same 401.
    let h = harness(Ok(invalid()));
    let (r, _) = h.run(&route, &params, Ok(jwt())).await;
    assert_eq!(r.as_ref().err().map(Rejection::status), Some(401));

    // A key of the wrong shape never reaches identity.
    let h = harness(Ok(ok()));
    let (r, cred) = h
        .run(&route, &params, Ok(Presented::Key("plat5-sk-1-old".into())))
        .await;
    assert_eq!(
        r.err(),
        Some(Rejection::Unauthorized(AuthError::InvalidKey))
    );
    assert_eq!(cred.key_prefix, None);
    assert_eq!(h.admit.calls.load(Ordering::SeqCst), 0);
    assert_eq!(h.counter.buckets(), vec![IP_BUCKET]);

    // No credential, or a bad JWT → 401 without identity; the JWT's reason in details.
    for err in [
        AuthError::MissingAuthorization,
        AuthError::InvalidToken {
            reason: "token_expired",
        },
        AuthError::MissingSubject,
    ] {
        let h = harness(Ok(ok()));
        let (r, _) = h.run(&route, &params, Err(err.clone())).await;
        let rejection = r.err().unwrap();
        assert_eq!(rejection, Rejection::Unauthorized(err.clone()));
        assert_eq!(h.admit.calls.load(Ordering::SeqCst), 0);
        assert_eq!(h.counter.buckets(), vec![IP_BUCKET]);
        let body = rejection.body(&route);
        assert_eq!(
            body.message,
            "Authentication required: a sign-in token or an API key."
        );
        assert_eq!(body.details, err.details());
    }

    // found: false, or no member in the tree → 403 before the rate limit.
    let mut no_member = ok();
    no_member.member_id = None;
    no_member.labels = vec![];
    for answer in [not_found(), no_member] {
        let h = harness(Ok(answer));
        let (r, _) = h.run(&route, &params, Ok(Presented::Key(key()))).await;
        assert_eq!(r.err(), Some(Rejection::NotFound));
        assert_eq!(
            h.counter.buckets(),
            vec![IP_BUCKET],
            "the tree's budget isn't spent"
        );
    }

    // No route label → passes the rate limit (spending it), then 403.
    let h = harness(Ok(answer_for(&route, &["project:write"])));
    let (r, cred) = h.run(&route, &params, Ok(Presented::Key(key()))).await;
    let passed = r.unwrap();
    assert_eq!(h.counter.buckets(), vec![route_bucket]);
    assert_eq!(
        authorize(&route, &passed.admitted),
        Err(Rejection::LabelDenied)
    );
    assert_eq!(cred.member_id.as_deref(), Some("m_1"));
    assert_eq!(cred.root_id.as_deref(), Some("n_organization"));
    assert_eq!(cred.node_id.as_deref(), Some("n_project"));
    assert_eq!(cred.principal.as_deref(), Some("person"));

    // Both 403s have one body, required_labels included.
    let nf = Rejection::NotFound.body(&route);
    let ld = Rejection::LabelDenied.body(&route);
    assert_eq!(nf.to_json_bytes(Some("r")), ld.to_json_bytes(Some("r")));
    assert_eq!(
        nf.details,
        Some(json!({ "required_labels": ["project:read"] }))
    );
    assert_eq!(Rejection::NotFound.status(), 403);

    // The label, or `*` → forwarded.
    for labels in [&["project:read"][..], &["*"], &["a", "project:read"]] {
        let h = harness(Ok(answer_for(&route, labels)));
        assert!(
            h.decide(&route, &params, Ok(Presented::Key(key())))
                .await
                .is_ok(),
            "{labels:?}"
        );
    }

    // A `[]` route admits any member, with no labels at all.
    let (list, list_params) = find(&snapshot, "GET", "/orgs/acme/projects");
    assert!(list.required_labels.is_empty());
    let h = harness(Ok(answer_for(list, &[])));
    assert!(h.decide(list, &list_params, Ok(jwt())).await.is_ok());

    // Identity down, non-2xx, or a missing field → 503, and nothing counted.
    let h = harness(Err("connection refused".into()));
    let (r, _) = h.run(&route, &params, Ok(Presented::Key(key()))).await;
    assert_eq!(r.err(), Some(Rejection::Unavailable));
    assert!(h.counter.buckets().is_empty());
    let mut short = ok();
    short.nodes.pop();
    let h = harness(Ok(short));
    let (r, _) = h.run(&route, &params, Ok(Presented::Key(key()))).await;
    assert_eq!(r.err(), Some(Rejection::Unavailable));

    // JWKS not loaded → 503.
    let h = harness(Ok(ok()));
    let (r, _) = h
        .run(&route, &params, Err(AuthError::JwtValidationUnavailable))
        .await;
    assert_eq!(r.err(), Some(Rejection::Unavailable));
    assert!(h.counter.buckets().is_empty());
}

#[tokio::test]
async fn the_failed_auth_limiter_answers_once_exceeded() {
    let snapshot = catalog();
    let (route, params) = project_read(&snapshot);
    let h = harness_with(
        Ok(not_found()),
        Limit {
            requests: 60,
            window_seconds: 60,
        },
        Limit {
            requests: 2,
            window_seconds: 30,
        },
    );
    for _ in 0..2 {
        let (r, _) = h.run(&route, &params, Ok(Presented::Key(key()))).await;
        assert_eq!(r.err(), Some(Rejection::NotFound));
    }
    let (r, _) = h.run(&route, &params, Ok(Presented::Key(key()))).await;
    assert_eq!(
        r.err(),
        Some(Rejection::RateLimited {
            retry_after: 30,
            info: None
        })
    );
    // Unmatched 404s count against the same limiter.
    assert_eq!(
        h.pipeline.note_unadmitted(IP).await,
        Some(Rejection::RateLimited {
            retry_after: 30,
            info: None
        })
    );
}

#[tokio::test]
async fn restore_admits_a_deleted_node_through_its_own_entry() {
    let snapshot = catalog();
    let (read, params) = project_read(&snapshot);
    let (restore, _) = find(&snapshot, "POST", "/orgs/acme/projects/web/restore");
    assert!(restore.resolve_deleted);
    let h = harness(Ok(not_found()));
    let (r, _) = h.run(&read, &params, Ok(Presented::Key(key()))).await;
    assert_eq!(r.err(), Some(Rejection::NotFound));
    // The deleted project resolves on the restore route; the read's cached
    // not-found must not answer it.
    h.set_answer(answer_for(restore, &["project:delete"]));
    assert!(h
        .decide(restore, &params, Ok(Presented::Key(key())))
        .await
        .is_ok());
    assert_eq!(h.admit.calls.load(Ordering::SeqCst), 2);
}

#[tokio::test]
async fn sign_in_routes_send_the_directory_member_and_no_nodes() {
    let snapshot = catalog();
    let (me, params) = find(&snapshot, "GET", "/me");
    let h = harness(Ok(AdmitAnswer {
        nodes: vec![],
        member_id: Some("m_dir".into()),
        ..admitted(&[], &[])
    }));
    let (admitted, fwd) = h.decide(me, &params, Ok(jwt())).await.unwrap();
    let req = h.admit.last.lock().unwrap().clone().unwrap();
    assert_eq!(req.scope, "directory");
    assert!(req.chain.is_empty());
    assert_eq!(fwd.path.as_deref(), Some("/members/m_dir"));
    assert_eq!(
        fwd.headers,
        vec![
            (MEMBER_ID_HEADER.to_string(), "m_dir".to_string()),
            (PRINCIPAL_HEADER.to_string(), "person".to_string()),
        ]
    );
    let Admitted::Caller(c) = admitted else {
        panic!()
    };
    assert_eq!(c.root_id(), None);
    assert_eq!(c.kid.as_deref(), Some("kid-1"));
    assert_eq!(
        h.counter.buckets(),
        vec!["GET /me m_dir"],
        "a sign-in route counts at the caller's directory member"
    );
    assert_eq!(
        Rejection::Unauthorized(AuthError::InvalidKey)
            .body(me)
            .message,
        "Authentication required: a sign-in token or a personal API key."
    );
}

fn policies_service() -> Value {
    json!({
        "e10s": {
            "url": "http://e10s:3000",
            "rate_limits": {
                "per-project": { "requests": 3, "window_seconds": 1, "count_at": "project" },
                "per-org": { "requests": 2, "window_seconds": 10, "count_at": "organization", "shared": true },
                "per-node": { "requests": 50, "window_seconds": 60 }
            },
            "routes": [{
                "path": "/orgs/{organization}/projects/{project}/check",
                "scope": "project",
                "upstream": "/projects/{subject.project_id}/check",
                "methods": ["POST"],
                "required_labels": ["check"],
                "rate_limit": ["per-project", "per-org", "per-node"]
            }, {
                "path": "/orgs/{organization}/projects/{project}/free",
                "scope": "project",
                "upstream": "/projects/{subject.project_id}/free",
                "methods": ["GET"],
                "required_labels": [],
                "rate_limit": false
            }, {
                "path": "/health",
                "scope": "public",
                "methods": ["GET"]
            }, {
                "path": "/forged",
                "scope": "public",
                "upstream": "/forged",
                "methods": ["GET"]
            }]
        }
    })
}

#[tokio::test]
async fn several_policies_each_count_at_their_node_and_all_must_pass() {
    let snapshot = snapshot_with(policies_service());
    let (route, params) = find(&snapshot, "POST", "/orgs/acme/projects/web/check");
    let h = harness(Ok(answer_for(route, &["check"])));

    let (r, _) = h.run(route, &params, Ok(Presented::Key(key()))).await;
    let passed = r.unwrap();
    assert_eq!(
        h.counter.buckets(),
        vec![
            "e10s:per-project:n_project",
            "per-org:n_organization",
            "e10s:per-node:n_project",
        ]
    );
    // The tightest: the fewest left. per-org has 1 of 2.
    assert_eq!(
        passed.rate_limit,
        Some(RateLimitInfo {
            limit: 2,
            remaining: 1,
            reset_epoch: 10
        })
    );

    // The org's budget runs out first; every policy still counts.
    let (r, _) = h.run(route, &params, Ok(Presented::Key(key()))).await;
    assert!(r.is_ok());
    let (r, _) = h.run(route, &params, Ok(Presented::Key(key()))).await;
    assert_eq!(
        r.err(),
        Some(Rejection::RateLimited {
            retry_after: 10,
            info: Some(RateLimitInfo {
                limit: 2,
                remaining: 0,
                reset_epoch: 10
            }),
        })
    );
    assert_eq!(h.counter.count("e10s:per-project:n_project"), 3);
    assert_eq!(h.counter.count("e10s:per-node:n_project"), 3);

    // Two exceeded: the longest wait.
    let (r, _) = h.run(route, &params, Ok(Presented::Key(key()))).await;
    let Err(Rejection::RateLimited { retry_after, info }) = r else {
        panic!()
    };
    assert_eq!(retry_after, 10);
    assert_eq!(info.unwrap().remaining, 0);
}

#[tokio::test]
async fn fallback_unlimited_and_public_buckets() {
    let snapshot = snapshot_with(policies_service());

    // The fallback counts at the route's own node.
    let (route, params) = project_read(&snapshot);
    let h = harness(Ok(answer_for(&route, &["project:read"])));
    h.run(&route, &params, Ok(Presented::Key(key())))
        .await
        .0
        .unwrap();
    assert_eq!(
        h.counter.buckets(),
        vec!["GET /orgs/{organization}/projects/{project} n_project"]
    );

    // `rate_limit: false` counts nowhere; a fallback of 0 is unlimited.
    let (free, free_params) = find(&snapshot, "GET", "/orgs/acme/projects/web/free");
    let h = harness(Ok(answer_for(free, &[])));
    let passed = h
        .run(free, &free_params, Ok(Presented::Key(key())))
        .await
        .0
        .unwrap();
    assert!(passed.rate_limit.is_none() && h.counter.buckets().is_empty());
    let h = harness_with(
        Ok(answer_for(&route, &["project:read"])),
        Limit {
            requests: 0,
            window_seconds: 60,
        },
        Limit {
            requests: 60,
            window_seconds: 60,
        },
    );
    let passed = h
        .run(&route, &params, Ok(Presented::Key(key())))
        .await
        .0
        .unwrap();
    assert!(passed.rate_limit.is_none() && h.counter.buckets().is_empty());

    // A public route skips admit, counts by client IP and sends no subject.
    let (health, health_params) = find(&snapshot, "GET", "/health");
    let h = harness(Ok(invalid()));
    let (admitted, fwd) = h
        .decide(health, &health_params, Err(AuthError::MissingAuthorization))
        .await
        .unwrap();
    assert_eq!(admitted, Admitted::Public);
    assert_eq!(h.admit.calls.load(Ordering::SeqCst), 0);
    assert_eq!(h.counter.buckets(), vec![format!("GET /health {IP}")]);
    assert_eq!(
        fwd,
        Forward {
            path: None,
            headers: vec![]
        }
    );
}

#[tokio::test]
async fn a_bad_path_param_is_400_and_a_bad_subject_id_500() {
    let snapshot = catalog();
    let (route, mut params) = find(&snapshot, "DELETE", "/orgs/acme/projects/web/bindings/b_1");
    params.insert("binding_id".into(), "b/1".into());
    let h = harness(Ok(answer_for(route, &["*"])));
    let err = h
        .decide(route, &params, Ok(Presented::Key(key())))
        .await
        .err();
    assert_eq!(err.as_ref().map(Rejection::status), Some(400));

    let (route, params) = project_read(&snapshot);
    let mut bad = answer_for(&route, &["*"]);
    bad.nodes[1] = NodeRef {
        kind: "project".into(),
        id: "n/x".into(),
    };
    let h = harness(Ok(bad));
    let err = h
        .decide(&route, &params, Ok(Presented::Key(key())))
        .await
        .err();
    assert_eq!(err.as_ref().map(Rejection::status), Some(500));
}
