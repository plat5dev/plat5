//! The pipeline against the catalog fixture
//! (services/control-plane/fixtures/catalog/gateway-feed.json): every route through
//! match, admit, limits, the permission check and the fill, and every row of the
//! admit table (docs/reference/gateway.md#admit).

use std::collections::{BTreeMap, HashMap};
use std::sync::atomic::Ordering;
use std::sync::{Arc, Mutex};
use std::time::Duration;

use serde_json::{json, Value};

use super::*;
use crate::auth::admit::tests::{
    admitted, invalid, not_in_tree, user, FakeAdmit, KEY_ID, MEMBER, USER,
};
use crate::auth::admit::AdmitAnswer;
use crate::control_plane::{build, Snapshot};
use crate::rate_limit::RateLimitInfo;
use crate::route_map::Required;

const IP: &str = "203.0.113.7";
const IP_BUCKET: &str = "@auth-failure:203.0.113.7";

fn key() -> Presented {
    Presented::Key("plat5-k-2-AAAA".into())
}

fn jwt() -> Presented {
    Presented::Jwt {
        issuer: "https://auth".into(),
        subject: "sub-1".into(),
        name: Some("Dana".into()),
        kid: Some("kid-1".into()),
    }
}

pub fn fixture(path: &str) -> Value {
    let path = format!(
        "{}/../control-plane/fixtures/{path}",
        env!("CARGO_MANIFEST_DIR")
    );
    serde_json::from_str(&std::fs::read_to_string(&path).expect("fixture")).unwrap()
}

/// The catalog feed, with `extra` services and `rate_limits` added.
fn snapshot_with(extra_services: Value, extra_limits: Value) -> Snapshot {
    let mut feed = fixture("catalog/gateway-feed.json");
    for (name, svc) in extra_services.as_object().unwrap() {
        feed["services"][name] = svc.clone();
    }
    for (name, policy) in extra_limits.as_object().unwrap() {
        feed["rate_limits"][name] = policy.clone();
    }
    build(feed).expect("the catalog fixture builds")
}

fn catalog() -> Snapshot {
    snapshot_with(json!({}), json!({}))
}

/// Fixed-window counters in memory: every bucket checked at once, counted only
/// if all pass. Records each bucket counted.
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
    async fn check(&self, buckets: &[Bucket]) -> Result<Verdict, Unavailable> {
        let mut counts = self.counts.lock().unwrap();
        let full = |b: &Bucket| counts.get(&b.key).copied().unwrap_or(0) >= b.requests;
        let allowed = !buckets.iter().any(full);
        let mut infos = Vec::new();
        let mut retry_after = 0;
        for b in buckets {
            let n = counts.entry(b.key.clone()).or_default();
            let was_full = *n >= b.requests;
            if allowed {
                *n += 1;
                self.order.lock().unwrap().push(b.key.clone());
            } else if was_full {
                retry_after = retry_after.max(b.window_seconds);
            }
            infos.push(RateLimitInfo {
                limit: b.requests,
                remaining: if was_full && !allowed {
                    0
                } else {
                    b.requests.saturating_sub(*n)
                },
                reset_epoch: b.window_seconds,
            });
        }
        Ok(if allowed {
            Verdict::Allowed(infos)
        } else {
            Verdict::Exceeded { retry_after, infos }
        })
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
    )
}

fn harness_with(answer: Result<AdmitAnswer, String>, auth_failure: Limit) -> Harness {
    let admit = Arc::new(FakeAdmit::new(answer));
    let counter = Arc::new(MemCounter::default());
    Harness {
        pipeline: Pipeline::new(
            AdmitCache::new(
                admit.clone(),
                Duration::from_secs(300),
                Duration::from_secs(10),
            ),
            counter.clone(),
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

    fn calls(&self) -> usize {
        self.admit.calls.load(Ordering::SeqCst)
    }

    fn last_request(&self) -> AdmitRequest {
        self.admit.last.lock().unwrap().clone().unwrap()
    }

    async fn run(
        &self,
        matched: &Matched,
        presented: Result<Presented, AuthError>,
    ) -> (Result<Passed, Rejection>, RequestCredential) {
        let mut cred = RequestCredential::default();
        let r = self
            .pipeline
            .admit_and_limit(matched, IP, presented, &mut cred)
            .await;
        (r, cred)
    }

    /// The whole decision: admit and limit, the permission check, and the fill.
    async fn decide(
        &self,
        matched: &Matched,
        presented: Result<Presented, AuthError>,
    ) -> Result<(Admitted, Forward), Rejection> {
        let passed = self.run(matched, presented).await.0?;
        authorize(&matched.route, &passed.admitted)?;
        let fwd = forward(matched, &passed.admitted)?;
        Ok((passed.admitted, fwd))
    }
}

/// A concrete request path for a route: a slug per chain kind, a value per own param.
fn concrete(route: &Route) -> String {
    route
        .path
        .split('/')
        .map(
            |seg| match seg.strip_prefix('{').and_then(|s| s.strip_suffix('}')) {
                Some(name) if route.chain.iter().any(|k| k == name) => {
                    format!("{}-slug", name.replace('_', "-"))
                }
                Some(name) => format!("01v{}", name.replace('_', "")),
                None => seg.to_string(),
            },
        )
        .collect::<Vec<_>>()
        .join("/")
}

fn node_id(kind: &str) -> String {
    format!("01n{kind}")
}

/// The answer for a route's caller: the chain's nodes on a kind route, the user on
/// a user route.
fn answer_for(route: &Route, permissions: &[&str]) -> AdmitAnswer {
    if route.scope == Scope::User {
        return user();
    }
    let ids: Vec<(String, String)> = route
        .chain
        .iter()
        .map(|k| (k.clone(), node_id(k)))
        .collect();
    let pairs: Vec<(&str, &str)> = ids.iter().map(|(k, i)| (k.as_str(), i.as_str())).collect();
    admitted(&pairs, permissions)
}

fn find(snapshot: &Snapshot, method: &str, path: &str) -> Matched {
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

/// The upstream with each placeholder replaced by what the gateway sends.
fn expected_upstream(
    feed_upstream: &str,
    route: &Route,
    params: &BTreeMap<String, String>,
) -> String {
    let mut want = feed_upstream.to_string();
    for kind in &route.chain {
        want = want.replace(&format!("{{{kind}.id}}"), &node_id(kind));
    }
    want = want
        .replace("{member.id}", MEMBER)
        .replace("{user.id}", USER);
    for (name, value) in params {
        want = want.replace(&format!("{{{name}}}"), value);
    }
    want
}

#[tokio::test]
async fn every_catalog_route_matches_admits_limits_and_fills_its_upstream() {
    let feed = fixture("catalog/gateway-feed.json");
    let snapshot = build(feed.clone()).unwrap();
    let mut seen = 0;
    let mut restore_routes = 0;
    for (service, svc) in feed["services"].as_object().unwrap() {
        for doc in svc["routes"].as_array().unwrap() {
            seen += 1;
            let method = doc["method"].as_str().unwrap();
            let template = doc["path"].as_str().unwrap();
            let probe = snapshot
                .routes
                .routes()
                .find(|r| r.path == template && r.method == method)
                .unwrap()
                .clone();
            let path = concrete(&probe);
            let matched = find(&snapshot, method, &path);
            let route = matched.route.clone();
            assert_eq!(
                (route.service.as_str(), route.path.as_str()),
                (service.as_str(), template),
                "{method} {path} matched another route"
            );

            let h = harness(Ok(answer_for(&route, &["*"])));
            let (admitted, fwd) = h
                .decide(&matched, Ok(key()))
                .await
                .unwrap_or_else(|r| panic!("{method} {template} refused: {r:?}"));

            // Admit got the credential as presented, the chain as written, and allow_deleted.
            let req = h.last_request();
            assert_eq!(
                req.credential,
                AdmitCredential::Credential("plat5-k-2-AAAA".into())
            );
            let refs: Vec<(String, String)> = req
                .chain
                .iter()
                .map(|c| (c.kind.clone(), c.reference.clone()))
                .collect();
            let want: Vec<(String, String)> = route
                .chain
                .iter()
                .map(|k| (k.clone(), format!("{k}-slug")))
                .collect();
            assert_eq!(refs, want, "{template}");
            assert_eq!(req.allow_deleted, doc["allow_deleted"].as_bool().unwrap());
            if template.ends_with("/restore") {
                restore_routes += 1;
                assert!(req.allow_deleted, "{template} must admit a deleted node");
            }

            // The default policy, per caller: at the node for a member, by user id on user routes.
            let bucket = match &route.scope {
                Scope::User => format!("default:user:{USER}"),
                _ => format!("default:{}:{MEMBER}", node_id(route.chain.last().unwrap())),
            };
            assert_eq!(h.counter.buckets(), vec![bucket], "{template}");

            // The upstream is the template with ids: no slug ever reaches a service.
            let want_path =
                expected_upstream(doc["upstream"].as_str().unwrap(), &route, &matched.params);
            assert!(!want_path.contains('{'), "{template}: {want_path}");
            assert_eq!(fwd.path, want_path, "{template}");
            for kind in &route.chain {
                assert!(
                    !fwd.path.contains("-slug"),
                    "{template}: slug of {kind} in upstream"
                );
            }

            // Headers: the chain's ids, the member and the principal; or the user.
            let Admitted::Caller(caller) = admitted else {
                panic!("{template} admitted as public")
            };
            match &route.scope {
                Scope::Kind(kind) => {
                    assert_eq!(route.chain.last(), Some(kind));
                    assert_eq!(caller.nodes.len(), route.chain.len());
                    for kind in &route.chain {
                        assert_eq!(
                            header(&fwd, &crate::kinds::id_header(kind)),
                            Some(node_id(kind).as_str())
                        );
                    }
                    assert_eq!(header(&fwd, MEMBER_ID_HEADER), Some(MEMBER));
                    assert_eq!(header(&fwd, USER_ID_HEADER), None);
                    assert_eq!(fwd.headers.len(), route.chain.len() + 2, "{template}");
                }
                Scope::User => {
                    assert!(req.chain.is_empty());
                    assert_eq!(header(&fwd, USER_ID_HEADER), Some(USER));
                    assert_eq!(header(&fwd, MEMBER_ID_HEADER), None);
                    assert_eq!(fwd.headers.len(), 2, "{template}");
                }
                Scope::Public => panic!("the catalog has no public routes"),
            }
            assert_eq!(header(&fwd, PRINCIPAL_HEADER), Some("person"));
        }
    }
    assert_eq!(seen, 85, "identity's 81 routes and audit's 4");
    assert_eq!(snapshot.routes.route_count(), seen);
    assert_eq!(restore_routes, 3, "a restore route per kind");
}

#[tokio::test]
async fn required_permissions_gate_their_routes() {
    let snapshot = catalog();
    let mut any_member = 0;
    for route in snapshot.routes.routes() {
        let matched = find(&snapshot, &route.method, &concrete(route));
        match &route.required {
            Required::AnyOf(list) => {
                for permission in list {
                    let h = harness(Ok(answer_for(route, &[permission.as_str()])));
                    assert!(
                        h.decide(&matched, Ok(key())).await.is_ok(),
                        "{}",
                        route.path
                    );
                }
                let h = harness(Ok(answer_for(route, &["some:other-permission"])));
                assert_eq!(
                    h.decide(&matched, Ok(key())).await.err(),
                    Some(Rejection::Denied),
                    "{}",
                    route.path
                );
            }
            Required::AnyMember => {
                any_member += 1;
                let h = harness(Ok(answer_for(route, &[])));
                assert!(
                    h.decide(&matched, Ok(jwt())).await.is_ok(),
                    "{}",
                    route.path
                );
            }
            Required::Nothing => assert_eq!(route.scope, Scope::User, "{}", route.path),
        }
    }
    assert!(
        any_member >= 10,
        "child lists, membership, roles: {any_member}"
    );
}

/// `GET /orgs/{organization}/projects/{project}`: `project:read`.
fn project_read(snapshot: &Snapshot) -> Matched {
    let m = find(snapshot, "GET", "/orgs/acme/projects/web");
    assert_eq!(
        m.route.required,
        Required::AnyOf(vec!["project:read".into()])
    );
    m
}

#[tokio::test]
async fn admit_table() {
    let snapshot = catalog();
    let read = project_read(&snapshot);
    let route_bucket = format!("default:{}:{MEMBER}", node_id("project"));
    let ok = || answer_for(&read.route, &["project:read"]);

    // valid: false → 401, counted on the failed-auth limiter, before the rate limit.
    for presented in [key(), jwt()] {
        let h = harness(Ok(invalid()));
        let (r, cred) = h.run(&read, Ok(presented)).await;
        assert_eq!(
            r.err(),
            Some(Rejection::Unauthorized(AuthError::InvalidCredential))
        );
        assert_eq!(cred.key_id, None);
        assert_eq!(h.counter.buckets(), vec![IP_BUCKET]);
    }

    // No credential, or a JWT refused locally → 401 without identity, not counted;
    // the JWT's reason in details.
    for err in [
        AuthError::MissingAuthorization,
        AuthError::InvalidAuthorizationHeader,
        AuthError::InvalidToken {
            reason: "token_expired",
        },
        AuthError::MissingSubject,
    ] {
        let h = harness(Ok(ok()));
        let (r, _) = h.run(&read, Err(err.clone())).await;
        let rejection = r.err().unwrap();
        assert_eq!(rejection, Rejection::Unauthorized(err.clone()));
        assert_eq!(h.calls(), 0);
        assert!(h.counter.buckets().is_empty(), "{err:?} is not counted");
        let body = rejection.body(&read.route);
        assert_eq!(
            body.message,
            "Authentication required: a sign-in token or an API key."
        );
        assert_eq!(body.details, err.details());
    }

    // member_id: null (no such node, or not in the tree) → 403 before the rate limit.
    let h = harness(Ok(not_in_tree()));
    let (r, cred) = h.run(&read, Ok(key())).await;
    assert_eq!(r.err(), Some(Rejection::NotFound));
    assert_eq!(cred.key_id.as_deref(), Some(KEY_ID));
    assert_eq!(
        h.counter.buckets(),
        vec![IP_BUCKET],
        "the tree's budget isn't spent"
    );

    // No route permission → passes the rate limit (spending it), then 403.
    let h = harness(Ok(answer_for(&read.route, &["project:write"])));
    let (r, cred) = h.run(&read, Ok(key())).await;
    let passed = r.unwrap();
    assert_eq!(h.counter.buckets(), vec![route_bucket.clone()]);
    assert_eq!(
        authorize(&read.route, &passed.admitted),
        Err(Rejection::Denied)
    );
    assert_eq!(
        cred,
        RequestCredential {
            auth_type: Some(AuthType::ApiKey),
            key_id: Some(KEY_ID.into()),
            principal: Some("person".into()),
            root_id: Some(node_id("organization")),
            node_id: Some(node_id("project")),
            member_id: Some(MEMBER.into()),
            user_id: None,
        }
    );

    // Both 403s have one body, required_permissions included.
    let nf = Rejection::NotFound.body(&read.route);
    let denied = Rejection::Denied.body(&read.route);
    assert_eq!(nf.to_json_bytes(Some("r")), denied.to_json_bytes(Some("r")));
    assert_eq!(
        nf.details,
        Some(json!({ "required_permissions": ["project:read"] }))
    );

    // The permission, or `*` → forwarded.
    for permissions in [&["project:read"][..], &["*"], &["a", "project:read"]] {
        let h = harness(Ok(answer_for(&read.route, permissions)));
        assert!(h.decide(&read, Ok(key())).await.is_ok(), "{permissions:?}");
    }

    // An any_member route admits a member with no permissions at all.
    let list = find(&snapshot, "GET", "/orgs/acme/projects");
    assert_eq!(list.route.required, Required::AnyMember);
    let h = harness(Ok(answer_for(&list.route, &[])));
    assert!(h.decide(&list, Ok(jwt())).await.is_ok());
    // Its 403 only means the caller isn't in the tree, so it names no permissions.
    let h = harness(Ok(not_in_tree()));
    let (r, _) = h.run(&list, Ok(key())).await;
    assert_eq!(r.err(), Some(Rejection::NotFound));
    assert_eq!(Rejection::NotFound.body(&list.route).details, None);

    // Identity down, or an answer missing a node → 503, and nothing counted.
    let h = harness(Err("connection refused".into()));
    let (r, _) = h.run(&read, Ok(key())).await;
    assert_eq!(r.err(), Some(Rejection::Unavailable));
    assert!(h.counter.buckets().is_empty());
    let mut short = ok();
    short.nodes.pop();
    let h = harness(Ok(short));
    let (r, _) = h.run(&read, Ok(key())).await;
    assert_eq!(r.err(), Some(Rejection::Unavailable));

    // JWKS not loaded → 503.
    let h = harness(Ok(ok()));
    let (r, _) = h.run(&read, Err(AuthError::JwtValidationUnavailable)).await;
    assert_eq!(r.err(), Some(Rejection::Unavailable));
    assert!(h.counter.buckets().is_empty());
}

#[tokio::test]
async fn a_jwt_is_sent_to_admit_as_its_issuer_subject_and_name() {
    let snapshot = catalog();
    let read = project_read(&snapshot);
    let h = harness(Ok(AdmitAnswer {
        key_id: None,
        ..answer_for(&read.route, &["project:read"])
    }));
    let (admitted, _) = h.decide(&read, Ok(jwt())).await.unwrap();
    assert_eq!(
        h.last_request().credential,
        AdmitCredential::Idp {
            issuer: "https://auth".into(),
            subject: "sub-1".into(),
            name: Some("Dana".into()),
        }
    );
    let Admitted::Caller(c) = admitted else {
        panic!()
    };
    assert_eq!(
        (c.auth_type, c.kid.as_deref(), c.key_id),
        (AuthType::Jwt, Some("kid-1"), None)
    );
}

#[tokio::test]
async fn the_failed_auth_limiter_answers_once_exceeded() {
    let snapshot = catalog();
    let read = project_read(&snapshot);
    let h = harness_with(
        Ok(not_in_tree()),
        Limit {
            requests: 2,
            window_seconds: 30,
        },
    );
    for _ in 0..2 {
        let (r, _) = h.run(&read, Ok(key())).await;
        assert_eq!(r.err(), Some(Rejection::NotFound));
    }
    let limited = Some(Rejection::RateLimited {
        retry_after: 30,
        info: None,
    });
    let (r, _) = h.run(&read, Ok(key())).await;
    assert_eq!(r.err(), limited);
    // A JWT refused locally still answers 401: it costs no Valkey trip.
    let (r, _) = h
        .run(
            &read,
            Err(AuthError::InvalidToken {
                reason: "token_expired",
            }),
        )
        .await;
    assert_eq!(r.err().map(|r| r.status()), Some(401));

    // Off at 0.
    let h = harness_with(
        Ok(invalid()),
        Limit {
            requests: 0,
            window_seconds: 60,
        },
    );
    let (r, _) = h.run(&read, Ok(key())).await;
    assert_eq!(r.err().map(|r| r.status()), Some(401));
    assert!(h.counter.buckets().is_empty());
}

#[tokio::test]
async fn restore_admits_a_deleted_node_through_its_own_entry() {
    let snapshot = catalog();
    let read = project_read(&snapshot);
    let restore = find(&snapshot, "POST", "/orgs/acme/projects/web/restore");
    assert!(restore.route.allow_deleted);
    let h = harness(Ok(not_in_tree()));
    let (r, _) = h.run(&read, Ok(key())).await;
    assert_eq!(r.err(), Some(Rejection::NotFound));
    // The deleted project resolves on the restore route; the read's cached
    // refusal must not answer it.
    h.set_answer(answer_for(&restore.route, &["project:delete"]));
    assert!(h.decide(&restore, Ok(key())).await.is_ok());
    assert_eq!(h.calls(), 2);
}

#[tokio::test]
async fn user_routes_send_the_user_and_no_nodes() {
    let snapshot = catalog();
    let me = find(&snapshot, "GET", "/user");
    let h = harness(Ok(user()));
    let (admitted, fwd) = h.decide(&me, Ok(jwt())).await.unwrap();
    assert!(h.last_request().chain.is_empty());
    assert_eq!(fwd.path, format!("/users/{USER}"));
    assert_eq!(
        fwd.headers,
        vec![
            (USER_ID_HEADER.to_string(), USER.to_string()),
            (PRINCIPAL_HEADER.to_string(), "person".to_string()),
        ]
    );
    let Admitted::Caller(c) = admitted else {
        panic!()
    };
    assert_eq!((c.root_id(), c.member_id.as_deref()), (None, None));
    assert_eq!(h.counter.buckets(), vec![format!("default:user:{USER}")]);

    // A service account's key: identity answers valid: false, and it's a 401.
    let h = harness(Ok(invalid()));
    let (r, _) = h.run(&me, Ok(key())).await;
    let rejection = r.err().unwrap();
    assert_eq!(rejection.status(), 401);
    assert_eq!(
        rejection.body(&me.route).message,
        "Authentication required: a sign-in token or a personal API key."
    );
}

fn e10s() -> Value {
    let route =
        |path: &str, method: &str, scope: &str, upstream: &str, required: Value, limits: Value| {
            json!({ "path": path, "method": method, "scope": scope, "upstream": upstream,
                "required_permissions": required, "rate_limit": limits,
                "audit": false, "allow_deleted": false })
        };
    json!({
        "e10s": {
            "url": "http://e10s:3000",
            "timeouts": { "connect": "2s", "read": "30s" },
            "max_body_bytes": 1048576,
            "routes": [
                route("/orgs/{organization}/projects/{project}/check", "POST", "project",
                      "/projects/{project.id}/check", json!(["check"]),
                      json!(["evals-caller", "evals-org"])),
                route("/orgs/{organization}/projects/{project}/free", "GET", "project",
                      "/projects/{project.id}/free", json!("any_member"), json!([])),
                route("/orgs/{organization}/projects/{project}/plans/{plan_id}", "GET", "project",
                      "/projects/{project.id}/plans/{plan_id}", json!(["project:read"]), json!([])),
                route("/health", "GET", "public", "/health", json!(null), json!(["default"])),
            ]
        }
    })
}

fn e10s_limits() -> Value {
    json!({
        "evals-caller": { "requests": 3, "window_seconds": 1 },
        "evals-org": { "requests": 2, "window_seconds": 10, "count_at": "organization" }
    })
}

#[tokio::test]
async fn policies_count_per_caller_or_at_count_at_all_at_once() {
    let snapshot = snapshot_with(e10s(), e10s_limits());
    let check = find(&snapshot, "POST", "/orgs/acme/projects/web/check");
    let h = harness(Ok(answer_for(&check.route, &["check"])));
    let caller_bucket = format!("evals-caller:{}:{MEMBER}", node_id("project"));
    let org_bucket = format!("evals-org:{}", node_id("organization"));

    let passed = h.run(&check, Ok(key())).await.0.unwrap();
    assert_eq!(
        h.counter.buckets(),
        vec![caller_bucket.clone(), org_bucket.clone()]
    );
    // The tightest: the fewest left. The org has 1 of 2.
    assert_eq!(
        passed.rate_limit,
        Some(RateLimitInfo {
            limit: 2,
            remaining: 1,
            reset_epoch: 10
        })
    );

    // The org's budget runs out first; the refused request spends nothing.
    assert!(h.run(&check, Ok(key())).await.0.is_ok());
    let (r, _) = h.run(&check, Ok(key())).await;
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
    assert_eq!(
        h.counter.count(&caller_bucket),
        2,
        "refused requests spend nothing"
    );
    assert_eq!(h.counter.count(&org_bucket), 2);
}

#[tokio::test]
async fn no_policy_counts_nowhere_and_public_counts_by_ip() {
    let snapshot = snapshot_with(e10s(), e10s_limits());

    let free = find(&snapshot, "GET", "/orgs/acme/projects/web/free");
    let h = harness(Ok(answer_for(&free.route, &[])));
    let passed = h.run(&free, Ok(key())).await.0.unwrap();
    assert!(passed.rate_limit.is_none() && h.counter.buckets().is_empty());

    // A public route skips admit, counts by client IP and sends no caller.
    let health = find(&snapshot, "GET", "/health");
    let h = harness(Ok(invalid()));
    let (admitted, fwd) = h
        .decide(&health, Err(AuthError::MissingAuthorization))
        .await
        .unwrap();
    assert_eq!(admitted, Admitted::Public);
    assert_eq!(h.calls(), 0);
    assert_eq!(h.counter.buckets(), vec![format!("default:ip:{IP}")]);
    assert_eq!(
        fwd,
        Forward {
            path: "/health".into(),
            headers: vec![]
        }
    );
}

#[tokio::test]
async fn a_param_that_would_change_the_upstream_path_is_400() {
    let snapshot = snapshot_with(e10s(), e10s_limits());
    for value in ["..", ".", "%2e%2e", "a%2Fb", "a%3Fb"] {
        let plan = find(
            &snapshot,
            "GET",
            &format!("/orgs/acme/projects/web/plans/{value}"),
        );
        assert_eq!(plan.params["plan_id"], value);
        let h = harness(Ok(answer_for(&plan.route, &["project:read"])));
        assert_eq!(
            h.decide(&plan, Ok(key())).await.err(),
            Some(Rejection::BadPathParam),
            "{value}"
        );
    }
    let plan = find(&snapshot, "GET", "/orgs/acme/projects/web/plans/pro.v2");
    let h = harness(Ok(answer_for(&plan.route, &["project:read"])));
    let (_, fwd) = h.decide(&plan, Ok(key())).await.unwrap();
    assert_eq!(
        fwd.path,
        format!("/projects/{}/plans/pro.v2", node_id("project"))
    );
    assert_eq!(Rejection::BadPathParam.status(), 400);
}

// Route checks (docs/reference/gateway.md#route-checks): the same match, admit
// and permission steps, per check.

/// Identity answering by request: the chain refs joined by `/` (empty on a user
/// route), with `+deleted` for `allow_deleted`. Anything else is not in the tree.
#[derive(Default)]
struct ByChain {
    answers: HashMap<String, AdmitAnswer>,
    down: bool,
    calls: std::sync::atomic::AtomicUsize,
    requests: Mutex<Vec<AdmitRequest>>,
}

impl ByChain {
    fn with(answers: &[(&str, AdmitAnswer)]) -> Arc<Self> {
        Arc::new(Self {
            answers: answers
                .iter()
                .map(|(k, a)| (k.to_string(), a.clone()))
                .collect(),
            ..Default::default()
        })
    }
}

impl crate::auth::admit::Admit for Arc<ByChain> {
    async fn admit(
        &self,
        req: &AdmitRequest,
    ) -> Result<AdmitAnswer, crate::auth::admit::AdmitUnavailable> {
        self.calls.fetch_add(1, Ordering::SeqCst);
        self.requests.lock().unwrap().push(req.clone());
        if self.down {
            return Err(crate::auth::admit::AdmitUnavailable("down".into()));
        }
        let mut key = req
            .chain
            .iter()
            .map(|c| c.reference.as_str())
            .collect::<Vec<_>>()
            .join("/");
        if req.allow_deleted {
            key.push_str("+deleted");
        }
        Ok(self.answers.get(&key).cloned().unwrap_or_else(not_in_tree))
    }
}

fn checks_pipeline(
    admit: Arc<ByChain>,
    auth_failure: Limit,
) -> (Pipeline<Arc<ByChain>, Arc<MemCounter>>, Arc<MemCounter>) {
    let counter = Arc::new(MemCounter::default());
    let pipeline = Pipeline::new(
        AdmitCache::new(admit, Duration::from_secs(300), Duration::from_secs(10)),
        counter.clone(),
        auth_failure,
    );
    (pipeline, counter)
}

const AUTH_FAILURE: Limit = Limit {
    requests: 60,
    window_seconds: 60,
};

fn checks(list: &[(&str, &str)]) -> Vec<Check> {
    list.iter()
        .map(|(method, path)| Check {
            method: method.to_string(),
            path: path.to_string(),
        })
        .collect()
}

fn key_credential() -> AdmitCredential {
    AdmitCredential::Credential("plat5-k-2-AAAA".into())
}

fn jwt_credential() -> AdmitCredential {
    jwt().into_admit().0
}

fn org(permissions: &[&str]) -> AdmitAnswer {
    admitted(&[("organization", "01norg")], permissions)
}

fn project(permissions: &[&str]) -> AdmitAnswer {
    admitted(
        &[("organization", "01norg"), ("project", "01nproject")],
        permissions,
    )
}

fn service_account(mut answer: AdmitAnswer) -> AdmitAnswer {
    answer.principal = Some("service_account".into());
    answer
}

async fn run_checks(
    pipeline: &Pipeline<Arc<ByChain>, Arc<MemCounter>>,
    snapshot: &Snapshot,
    credential: &AdmitCredential,
    list: &[(&str, &str)],
) -> Result<Vec<bool>, Rejection> {
    pipeline
        .check_routes(
            &snapshot.routes,
            &snapshot.route_checks.default,
            credential,
            &checks(list),
            IP,
        )
        .await
        .map(|c| c.allowed)
}

#[tokio::test]
async fn route_checks_answer_what_the_pipeline_would_decide() {
    let snapshot = snapshot_with(e10s(), e10s_limits());
    // A project-only member: in acme's tree with nothing at the org, project:read at web.
    let identity = ByChain::with(&[
        ("acme", org(&[])),
        ("acme/web", project(&["project:read"])),
        ("", user()),
    ]);
    let (p, _) = checks_pipeline(identity.clone(), AUTH_FAILURE);
    let got = run_checks(
        &p,
        &snapshot,
        &jwt_credential(),
        &[
            ("GET", "/orgs/acme/projects/web"),           // project:read
            ("GET", "/orgs/acme/projects/web/bindings"),  // project:bindings:read
            ("GET", "/orgs/acme/bindings"),               // organization:bindings:read
            ("GET", "/orgs/acme"),                        // organization:read
            ("GET", "/orgs/acme/projects"),               // any_member
            ("GET", "/orgs/acme/membership"),             // any_member
            ("GET", "/orgs/acme/projects/web/free"),      // any_member
            ("GET", "/orgs/acme/projects/web/plans/pro"), // project:read, own param
            ("GET", "/orgs/acme/projects/gone"),          // no such node
            ("GET", "/orgs/other"),                       // another tree
            ("GET", "/health"),                           // public
            ("GET", "/user"),                             // a person on a user route
            ("GET", "/nowhere"),                          // no route
            ("PUT", "/orgs/acme"),                        // a method the route doesn't take
            ("GET", "/orgs/acme/"),                       // not the route's path
        ],
    )
    .await
    .unwrap();
    assert_eq!(
        got,
        vec![
            true, false, false, false, true, true, true, true, false, false, true, true, false,
            false, false
        ]
    );
    // One admit per chain: acme, acme/web, acme/gone, other and the user route.
    assert_eq!(identity.calls.load(Ordering::SeqCst), 5);
    // The credential is the JWT's issuer and subject, as on a route.
    let requests = identity.requests.lock().unwrap();
    assert!(requests.iter().all(|r| r.credential == jwt_credential()));
    assert!(requests.iter().all(|r| !r.allow_deleted));
}

#[tokio::test]
async fn a_route_check_takes_every_permission_rule() {
    let snapshot = catalog();
    let identity = ByChain::with(&[
        ("acme", org(&["*"])),
        (
            "acme/web",
            project(&["project:write", "project:bindings:read"]),
        ),
    ]);
    let (p, _) = checks_pipeline(identity, AUTH_FAILURE);
    let got = run_checks(
        &p,
        &snapshot,
        &key_credential(),
        &[
            ("DELETE", "/orgs/acme"), // * meets any permission
            ("GET", "/orgs/acme/projects/web/bindings"),
            ("PATCH", "/orgs/acme/projects/web"),
            ("DELETE", "/orgs/acme/projects/web"), // project:delete: not held
        ],
    )
    .await
    .unwrap();
    assert_eq!(got, vec![true, true, true, false]);
}

#[tokio::test]
async fn a_restore_check_admits_the_deleted_node_as_the_route_does() {
    let snapshot = catalog();
    // web is deleted: only an allow_deleted admit finds it.
    let identity = ByChain::with(&[("acme/web+deleted", project(&["project:delete"]))]);
    let (p, _) = checks_pipeline(identity.clone(), AUTH_FAILURE);
    let got = run_checks(
        &p,
        &snapshot,
        &key_credential(),
        &[
            ("GET", "/orgs/acme/projects/web"),
            ("POST", "/orgs/acme/projects/web/restore"),
        ],
    )
    .await
    .unwrap();
    assert_eq!(got, vec![false, true]);
    let requests = identity.requests.lock().unwrap();
    assert_eq!(
        requests.iter().map(|r| r.allow_deleted).collect::<Vec<_>>(),
        vec![false, true]
    );
}

#[tokio::test]
async fn a_service_account_is_false_on_user_checks() {
    let snapshot = catalog();
    // Identity answers a service account on a user route valid: false.
    let identity = ByChain::with(&[("acme", service_account(org(&["*"]))), ("", invalid())]);
    let (p, counter) = checks_pipeline(identity, AUTH_FAILURE);
    let got = run_checks(
        &p,
        &snapshot,
        &key_credential(),
        &[("GET", "/user"), ("GET", "/orgs/acme"), ("POST", "/orgs")],
    )
    .await
    .unwrap();
    assert_eq!(got, vec![false, true, false]);
    assert_eq!(counter.count(IP_BUCKET), 0, "a false is no failed auth");
}

#[tokio::test]
async fn a_credential_admit_refuses_is_401_counted_once() {
    let snapshot = catalog();
    let identity = ByChain::with(&[
        ("acme", invalid()),
        ("acme/web", invalid()),
        ("", invalid()),
    ]);
    let (p, counter) = checks_pipeline(identity.clone(), AUTH_FAILURE);
    let tree_checks = [("GET", "/orgs/acme"), ("GET", "/orgs/acme/projects/web")];
    assert_eq!(
        run_checks(&p, &snapshot, &key_credential(), &tree_checks).await,
        Err(Rejection::Unauthorized(AuthError::InvalidCredential))
    );
    assert_eq!(counter.count(IP_BUCKET), 1);
    // The first refusal ends the call.
    assert_eq!(identity.calls.load(Ordering::SeqCst), 1);

    // A JWT refused on a user check: a suspended person, 401.
    assert_eq!(
        run_checks(&p, &snapshot, &jwt_credential(), &[("GET", "/user")]).await,
        Err(Rejection::Unauthorized(AuthError::InvalidCredential))
    );
    assert_eq!(counter.count(IP_BUCKET), 2);

    // The failed-auth limiter answers once it's full.
    let (p, _) = checks_pipeline(
        identity,
        Limit {
            requests: 1,
            window_seconds: 60,
        },
    );
    run_checks(&p, &snapshot, &key_credential(), &tree_checks)
        .await
        .unwrap_err();
    assert!(matches!(
        run_checks(&p, &snapshot, &key_credential(), &tree_checks).await,
        Err(Rejection::RateLimited { info: None, .. })
    ));
}

#[tokio::test]
async fn a_key_no_tree_check_admits_is_admitted_once_with_no_node() {
    let snapshot = snapshot_with(e10s(), e10s_limits());
    let only_user = [("GET", "/user"), ("POST", "/orgs")];
    // A revoked or unknown key: admit refuses it on any chain.
    let identity = ByChain::with(&[("", invalid()), ("-", invalid())]);
    let (p, counter) = checks_pipeline(identity.clone(), AUTH_FAILURE);
    assert_eq!(
        run_checks(&p, &snapshot, &key_credential(), &only_user).await,
        Err(Rejection::Unauthorized(AuthError::InvalidCredential))
    );
    assert_eq!(counter.count(IP_BUCKET), 1, "counted once");
    // Public and unmatched checks admit nothing either.
    assert_eq!(
        run_checks(
            &p,
            &snapshot,
            &key_credential(),
            &[("GET", "/health"), ("GET", "/nowhere")]
        )
        .await,
        Err(Rejection::Unauthorized(AuthError::InvalidCredential))
    );
    assert_eq!(counter.count(IP_BUCKET), 2);
    let probe = identity.requests.lock().unwrap().last().unwrap().clone();
    assert_eq!(probe.chain.len(), 1);
    assert_eq!(probe.chain[0].reference, "-");

    // A service account's key: refused on the user route, valid on a chain
    // that names no node, so its user checks are false and the call a 200.
    let identity = ByChain::with(&[("", invalid())]);
    let (p, counter) = checks_pipeline(identity.clone(), AUTH_FAILURE);
    assert_eq!(
        run_checks(&p, &snapshot, &key_credential(), &only_user).await,
        Ok(vec![false, false])
    );
    assert_eq!(counter.count(IP_BUCKET), 0, "a false is no failed auth");
    // A tree check that admitted the key needs no second admit.
    let identity = ByChain::with(&[("acme", service_account(org(&["*"]))), ("", invalid())]);
    let (p, _) = checks_pipeline(identity.clone(), AUTH_FAILURE);
    run_checks(
        &p,
        &snapshot,
        &key_credential(),
        &[("GET", "/user"), ("GET", "/orgs/acme")],
    )
    .await
    .unwrap();
    assert_eq!(identity.calls.load(Ordering::SeqCst), 2);
}

#[tokio::test]
async fn a_jwt_no_check_admits_is_admitted_as_on_a_user_route() {
    let snapshot = snapshot_with(e10s(), e10s_limits());
    // A suspended person with only public checks.
    let identity = ByChain::with(&[("", invalid())]);
    let (p, counter) = checks_pipeline(identity.clone(), AUTH_FAILURE);
    assert_eq!(
        run_checks(&p, &snapshot, &jwt_credential(), &[("GET", "/health")]).await,
        Err(Rejection::Unauthorized(AuthError::InvalidCredential))
    );
    assert_eq!(counter.count(IP_BUCKET), 1);
    assert!(identity.requests.lock().unwrap()[0].chain.is_empty());
    // A person who isn't suspended.
    let identity = ByChain::with(&[("", user())]);
    let (p, _) = checks_pipeline(identity, AUTH_FAILURE);
    assert_eq!(
        run_checks(&p, &snapshot, &jwt_credential(), &[("GET", "/health")]).await,
        Ok(vec![true])
    );
}

#[tokio::test]
async fn route_checks_count_default_once_a_call_per_credential() {
    let snapshot = snapshot_with(
        json!({}),
        json!({ "default": { "requests": 2, "window_seconds": 60 } }),
    );
    let identity = ByChain::with(&[("acme", org(&["*"]))]);
    let (p, counter) = checks_pipeline(identity, AUTH_FAILURE);
    let three = [("GET", "/orgs/acme"); 3];
    let call = |credential: AdmitCredential| {
        let p = &p;
        let snapshot = &snapshot;
        async move {
            p.check_routes(
                &snapshot.routes,
                &snapshot.route_checks.default,
                &credential,
                &checks(&three),
                IP,
            )
            .await
        }
    };
    let first = call(key_credential()).await.unwrap();
    assert_eq!(first.allowed, vec![true; 3]);
    assert_eq!(
        first.rate_limit.as_ref().map(|i| (i.limit, i.remaining)),
        Some((2, 1))
    );
    let bucket = format!(
        "default:credential:{}",
        crate::auth::admit::credential_hash(&key_credential())
    );
    assert_eq!(counter.buckets(), vec![bucket.clone()]);
    assert!(!bucket.contains("AAAA"), "never the key itself");

    call(key_credential()).await.unwrap();
    match call(key_credential()).await {
        Err(Rejection::RateLimited {
            retry_after: 60,
            info: Some(info),
        }) => assert_eq!((info.limit, info.remaining), (2, 0)),
        other => panic!("{other:?}"),
    }
    // Another credential has its own budget; a JWT's is its issuer and subject.
    call(jwt_credential()).await.unwrap();
    assert_eq!(counter.count(&bucket), 2);
}

#[tokio::test]
async fn route_checks_without_identity_are_503() {
    let snapshot = catalog();
    let identity = Arc::new(ByChain {
        down: true,
        ..Default::default()
    });
    let (p, _) = checks_pipeline(identity, AUTH_FAILURE);
    assert_eq!(
        run_checks(&p, &snapshot, &key_credential(), &[("GET", "/orgs/acme")]).await,
        Err(Rejection::Unavailable)
    );
    // Every call admits the credential, so a call of unmatched checks is 503 too.
    assert_eq!(
        run_checks(&p, &snapshot, &key_credential(), &[("GET", "/nowhere")]).await,
        Err(Rejection::Unavailable)
    );
}

#[test]
fn the_route_checks_credential_is_any_a_route_takes() {
    type P = Pipeline<Arc<ByChain>, Arc<MemCounter>>;
    assert_eq!(P::check_credential(Ok(key())), Ok(key_credential()));
    assert_eq!(P::check_credential(Ok(jwt())), Ok(jwt_credential()));
    assert_eq!(
        P::check_credential(Err(AuthError::MissingAuthorization)),
        Err(Rejection::Unauthorized(AuthError::MissingAuthorization))
    );
    assert_eq!(
        P::check_credential(Err(AuthError::JwtValidationUnavailable)),
        Err(Rejection::Unavailable)
    );
}
