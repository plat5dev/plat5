//! The pipeline against the catalog fixture
//! (services/control-plane/fixtures/catalog/gateway-feed.json): every route through
//! match, admit, the permission check, limits and the fill, and every row of the
//! admit table (docs/reference/gateway.md#admit).

use std::collections::{BTreeMap, HashMap};
use std::sync::atomic::Ordering;
use std::sync::{Arc, Mutex};
use std::time::Duration;

use serde_json::{json, Value};

use super::*;
use crate::auth::admit::tests::{
    admitted, invalid, not_in_tree, scoped_personal_user, service_account_user, user, FakeAdmit,
    KEY_ID, MEMBER, USER,
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
        scopes: None,
        policy_claims: BTreeMap::new(),
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
    down: std::sync::atomic::AtomicBool,
}

impl MemCounter {
    fn set_down(&self, down: bool) {
        self.down.store(down, std::sync::atomic::Ordering::SeqCst);
    }
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
        if self.down.load(std::sync::atomic::Ordering::SeqCst) {
            return Err(Unavailable);
        }
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
            .decide(matched, IP, presented, &mut cred)
            .await
            .map_err(|refused| refused.rejection);
        (r, cred)
    }

    /// The whole decision: admit, the permission check and limit, then the fill.
    async fn decide(
        &self,
        matched: &Matched,
        presented: Result<Presented, AuthError>,
    ) -> Result<(Admitted, Forward), Rejection> {
        let passed = self.run(matched, presented).await.0?;
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

            // The policy defaults.rate_limit names, per caller: at the node for a member, by user id on user routes.
            let bucket = match &route.scope {
                Scope::User => format!("standard:user:{USER}"),
                _ => format!("standard:{}:{MEMBER}", node_id(route.chain.last().unwrap())),
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
                    assert_eq!(caller.nodes().len(), route.chain.len());
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
    assert_eq!(seen, 84, "identity's 80 routes and audit's 4");
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
    // the JWT's reason in details, and auth_type recorded for locally-refused JWTs.
    for (err, want_auth_type) in [
        (AuthError::MissingAuthorization, None),
        (AuthError::InvalidAuthorizationHeader, None),
        (
            AuthError::InvalidToken {
                reason: "token_expired",
            },
            Some(AuthType::Jwt),
        ),
        (AuthError::MissingSubject, Some(AuthType::Jwt)),
    ] {
        let h = harness(Ok(ok()));
        let (r, cred) = h.run(&read, Err(err.clone())).await;
        let rejection = r.err().unwrap();
        assert_eq!(rejection, Rejection::Unauthorized(err.clone()));
        assert_eq!(cred.auth_type, want_auth_type, "{err:?}");
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

    // No route permission → 403 before the rate limit, counted like admit's.
    let h = harness(Ok(answer_for(&read.route, &["project:write"])));
    let mut cred = RequestCredential::default();
    let refused = h
        .pipeline
        .decide(&read, IP, Ok(key()), &mut cred)
        .await
        .unwrap_err();
    assert_eq!(refused.rejection, Rejection::Denied);
    assert_eq!(
        refused.caller.as_ref().and_then(|c| c.member_id()),
        Some(MEMBER),
        "the caller, to log and audit the denial in its tree"
    );
    assert_eq!(
        h.counter.buckets(),
        vec![IP_BUCKET],
        "the route's budget isn't spent"
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

    // Both 403s have one body, permissions included.
    let nf = Rejection::NotFound.body(&read.route);
    let denied = Rejection::Denied.body(&read.route);
    assert_eq!(nf.to_json_bytes(Some("r")), denied.to_json_bytes(Some("r")));
    assert_eq!(nf.details, Some(json!({ "permissions": ["project:read"] })));

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
            scopes: None,
            claims: BTreeMap::new(),
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
async fn a_full_failed_auth_bucket_answers_missing_and_forbidden_alike() {
    let snapshot = catalog();
    let missing = find(&snapshot, "GET", "/orgs/acme/projects/nope");
    let forbidden = project_read(&snapshot);
    let limit = Limit {
        requests: 3,
        window_seconds: 30,
    };

    // Under the limit: one 403, one body.
    let h = harness_with(Ok(not_in_tree()), limit);
    let (m, _) = h.run(&missing, Ok(key())).await;
    h.set_answer(answer_for(&forbidden.route, &["project:write"]));
    let (f, _) = h.run(&forbidden, Ok(key())).await;
    let (m, f) = (m.unwrap_err(), f.unwrap_err());
    assert_eq!((m.status(), f.status()), (403, 403));
    assert_eq!(
        m.body(&missing.route).to_json_bytes(Some("r")),
        f.body(&forbidden.route).to_json_bytes(Some("r"))
    );
    assert_eq!(h.counter.count(IP_BUCKET), 2, "both counted by the IP");

    // Fill the IP's bucket with missing slugs: then a missing node and a
    // forbidden one get the same 429, Retry-After only.
    let h = harness_with(Ok(not_in_tree()), limit);
    for slug in ["a-1", "a-2", "a-3"] {
        let path = format!("/orgs/acme/projects/{slug}");
        let (r, _) = h.run(&find(&snapshot, "GET", &path), Ok(key())).await;
        assert_eq!(r.err(), Some(Rejection::NotFound));
    }
    let (m, _) = h.run(&missing, Ok(key())).await;
    h.set_answer(answer_for(&forbidden.route, &["project:write"]));
    let (f, _) = h.run(&forbidden, Ok(key())).await;
    let limited = || {
        Some(Rejection::RateLimited {
            retry_after: 30,
            info: None,
        })
    };
    assert_eq!(m.err(), limited());
    assert_eq!(f.err(), limited());
    assert!(
        h.counter.buckets().iter().all(|b| b == IP_BUCKET),
        "no route bucket: neither reached the rate limit, so neither carries X-RateLimit-*"
    );

    // A permitted caller isn't counted by the IP's bucket: it passes.
    h.set_answer(answer_for(&forbidden.route, &["project:read"]));
    assert!(h.decide(&forbidden, Ok(jwt())).await.is_ok());
}

#[tokio::test]
async fn refused_callers_spend_no_count_at_budget() {
    let limits = json!({
        "evals-caller": { "requests": 3, "window": "1s" },
        "evals-org": { "requests": 2, "window": "10s", "count_at": "project" }
    });
    let snapshot = snapshot_with(e10s(), limits);
    let check = find(&snapshot, "POST", "/orgs/acme/projects/web/check");
    let project_bucket = format!("evals-org:{}", node_id("project"));

    // A member of the tree with no permission on the project, then a caller
    // outside the tree: 403s, again and again, and the project's budget untouched.
    let h = harness(Ok(answer_for(&check.route, &["project:read"])));
    for _ in 0..5 {
        let (r, _) = h.run(&check, Ok(key())).await;
        assert_eq!(r.err(), Some(Rejection::Denied));
    }
    h.set_answer(not_in_tree());
    for _ in 0..5 {
        let (r, _) = h.run(&check, Ok(jwt())).await;
        assert_eq!(r.err(), Some(Rejection::NotFound));
    }
    assert_eq!(h.counter.count(&project_bucket), 0);
    assert!(h.counter.buckets().iter().all(|b| b == IP_BUCKET));
    assert_eq!(h.counter.count(IP_BUCKET), 10);

    // The project's own callers still have its whole budget.
    let h2 = harness(Ok(answer_for(&check.route, &["check"])));
    let passed = h2.run(&check, Ok(key())).await.0.unwrap();
    assert_eq!(
        passed.rate_limit,
        Some(RateLimitInfo {
            limit: 2,
            remaining: 1,
            reset_epoch: 10
        })
    );
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
    assert_eq!((c.root_id(), c.member_id()), (None, None));
    assert_eq!(h.counter.buckets(), vec![format!("standard:user:{USER}")]);

    // A revoked or unknown key: identity answers valid: false, and it's a 401,
    // counted by the failed-auth limiter.
    let h = harness(Ok(invalid()));
    let (r, _) = h.run(&me, Ok(key())).await;
    let rejection = r.err().unwrap();
    assert_eq!(rejection.status(), 401);
    assert_eq!(
        rejection.body(&me.route).message,
        "Authentication required: a sign-in token or a personal API key."
    );
    assert_eq!(h.counter.buckets(), vec![IP_BUCKET]);
}

#[tokio::test]
async fn a_service_account_on_a_user_route_is_403() {
    let snapshot = catalog();
    let me = find(&snapshot, "GET", "/user");
    let h = harness(Ok(service_account_user()));
    let (r, cred) = h.run(&me, Ok(key())).await;
    let rejection = r.err().unwrap();
    assert_eq!(rejection, Rejection::ServiceAccount);
    assert_eq!(rejection.status(), 403);
    let body = rejection.body(&me.route);
    assert_eq!(
        (body.code.as_str(), body.message.as_str(), body.details),
        ("FORBIDDEN", "Service accounts can't use this route.", None)
    );
    // The credential is good: neither failed-auth nor user-route buckets are spent.
    assert!(h.counter.buckets().is_empty());
    assert_eq!(
        (cred.key_id.as_deref(), cred.principal.as_deref()),
        (Some(KEY_ID), Some("service_account"))
    );
    // Cached as an answer, not a refusal.
    h.run(&me, Ok(key())).await.0.unwrap_err();
    assert_eq!(h.calls(), 1);
}

fn e10s() -> Value {
    let route =
        |path: &str, method: &str, scope: &str, upstream: &str, required: Value, limits: Value| {
            json!({ "path": path, "method": method, "scope": scope, "upstream": upstream,
                "permissions": required, "rate_limit": limits,
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
                route("/health", "GET", "public", "/health", json!(null), json!(["standard"])),
            ]
        }
    })
}

fn e10s_limits() -> Value {
    json!({
        "evals-caller": { "requests": 3, "window": "1s" },
        "evals-org": { "requests": 2, "window": "10s", "count_at": "organization" }
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
    assert_eq!(h.counter.buckets(), vec![format!("standard:ip:{IP}")]);
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
            &snapshot.default_policies,
            credential,
            &checks(list),
            IP,
            None,
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
    // Identity answers a service account on a user route valid, with no user.
    let identity = ByChain::with(&[
        ("acme", service_account(org(&["*"]))),
        ("", service_account_user()),
    ]);
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
async fn api_key_ip_allowlist_enforced_in_gateway_with_shared_cache() {
    let snapshot = catalog();
    let read = project_read(&snapshot);
    let mut ans = answer_for(&read.route, &["project:read"]);
    ans.allowed_cidrs = vec!["203.0.113.0/24".into()];
    let h = harness(Ok(ans));

    // Allowed IP passes.
    let mut cred = RequestCredential::default();
    assert!(h
        .pipeline
        .decide(&read, "203.0.113.7", Ok(key()), &mut cred)
        .await
        .is_ok());
    assert_eq!(h.admit.calls.load(Ordering::SeqCst), 1);

    // Disallowed IP gets 401 and counts failed-auth limiter, reusing the cached admit.
    let mut cred = RequestCredential::default();
    let err = h
        .pipeline
        .decide(&read, "198.51.100.9", Ok(key()), &mut cred)
        .await
        .unwrap_err();
    assert_eq!(
        err.rejection,
        Rejection::Unauthorized(AuthError::InvalidCredential)
    );
    assert_eq!(h.counter.count("@auth-failure:198.51.100.9"), 1);
    assert_eq!(h.admit.calls.load(Ordering::SeqCst), 1);
}

#[tokio::test]
async fn api_key_ip_allowlist_refuses_route_checks_with_401() {
    let snapshot = catalog();
    let mut ans = org(&["*"]);
    ans.allowed_cidrs = vec!["10.0.0.0/8".into()];
    let identity = ByChain::with(&[("acme", ans)]);
    let (p, counter) = checks_pipeline(identity, AUTH_FAILURE);
    let err = run_checks(&p, &snapshot, &key_credential(), &[("GET", "/orgs/acme")])
        .await
        .unwrap_err();
    assert_eq!(err, Rejection::Unauthorized(AuthError::InvalidCredential));
    assert_eq!(counter.count(IP_BUCKET), 1);
}

#[tokio::test]
async fn route_checks_answer_denied_and_missing_false_uncounted() {
    let snapshot = catalog();
    let identity = ByChain::with(&[("acme/web", project(&["project:write"]))]);
    let (p, counter) = checks_pipeline(identity, AUTH_FAILURE);
    let checks = [
        ("GET", "/orgs/acme/projects/web"),
        ("PATCH", "/orgs/acme/projects/web"),
        ("GET", "/orgs/acme/projects/nope"),
    ];
    for _ in 0..3 {
        let got = run_checks(&p, &snapshot, &key_credential(), &checks)
            .await
            .unwrap();
        assert_eq!(got, vec![false, true, false]);
    }
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

    // Refused on a user check: a suspended person's JWT, a revoked key, 401.
    assert_eq!(
        run_checks(&p, &snapshot, &jwt_credential(), &[("GET", "/user")]).await,
        Err(Rejection::Unauthorized(AuthError::InvalidCredential))
    );
    assert_eq!(
        run_checks(&p, &snapshot, &key_credential(), &[("GET", "/user")]).await,
        Err(Rejection::Unauthorized(AuthError::InvalidCredential))
    );
    assert_eq!(counter.count(IP_BUCKET), 3);

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

/// Public, user and mixed batches, for a person, a revoked key and a
/// service account: each check admits as its request would.
#[tokio::test]
async fn route_check_batches_admit_as_their_requests_would() {
    let snapshot = snapshot_with(e10s(), e10s_limits());
    let public = [("GET", "/health"), ("GET", "/nowhere")];
    let only_user = [("GET", "/user"), ("POST", "/orgs")];
    let mixed = [("GET", "/health"), ("GET", "/user"), ("GET", "/orgs/acme")];
    let person = || ByChain::with(&[("", user()), ("acme", org(&["*"]))]);
    let revoked = || ByChain::with(&[("", invalid()), ("acme", invalid())]);
    let bot = || {
        ByChain::with(&[
            ("", service_account_user()),
            ("acme", service_account(org(&["*"]))),
        ])
    };
    let refused = || Err(Rejection::Unauthorized(AuthError::InvalidCredential));
    // (identity, credential, batch, answer, admits, failed auths counted)
    type Case<'a> = (
        Arc<ByChain>,
        AdmitCredential,
        &'a [(&'a str, &'a str)],
        Result<Vec<bool>, Rejection>,
        usize,
        u64,
    );
    let cases: Vec<Case> = vec![
        // Only public and unmatched checks: no admit, whatever the credential.
        (
            person(),
            jwt_credential(),
            &public,
            Ok(vec![true, false]),
            0,
            0,
        ),
        (
            revoked(),
            key_credential(),
            &public,
            Ok(vec![true, false]),
            0,
            0,
        ),
        (
            bot(),
            key_credential(),
            &public,
            Ok(vec![true, false]),
            0,
            0,
        ),
        // Only user checks: admitted with an empty chain, once (the cache).
        (
            person(),
            jwt_credential(),
            &only_user,
            Ok(vec![true, true]),
            1,
            0,
        ),
        (revoked(), key_credential(), &only_user, refused(), 1, 1),
        (
            bot(),
            key_credential(),
            &only_user,
            Ok(vec![false, false]),
            1,
            0,
        ),
        // Mixed: the user check's admit and the tree check's.
        (
            person(),
            jwt_credential(),
            &mixed,
            Ok(vec![true, true, true]),
            2,
            0,
        ),
        (revoked(), key_credential(), &mixed, refused(), 1, 1),
        (
            bot(),
            key_credential(),
            &mixed,
            Ok(vec![true, false, true]),
            2,
            0,
        ),
    ];
    for (i, (identity, credential, batch, want, admits, counted)) in cases.into_iter().enumerate() {
        let (p, counter) = checks_pipeline(identity.clone(), AUTH_FAILURE);
        assert_eq!(
            run_checks(&p, &snapshot, &credential, batch).await,
            want,
            "case {i}"
        );
        assert_eq!(
            identity.calls.load(Ordering::SeqCst),
            admits,
            "case {i}: admits"
        );
        assert_eq!(counter.count(IP_BUCKET), counted, "case {i}: counted");
        let requests = identity.requests.lock().unwrap();
        assert!(
            requests
                .iter()
                .all(|r| r.chain.iter().all(|c| c.kind != "-" && c.reference != "-")),
            "case {i}: every chain is a route's"
        );
    }
}

#[tokio::test]
async fn route_checks_count_the_defaults_once_a_call_per_credential() {
    let snapshot = snapshot_with(
        json!({}),
        json!({ "standard": { "requests": 2, "window": "60s" } }),
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
                &snapshot.default_policies,
                &credential,
                &checks(&three),
                IP,
                None,
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
        "standard:credential:{}",
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

// defaults.rate_limit names the policies route checks count, whatever their
// names: each counted per credential, checked together, the tightest reported.
#[tokio::test]
async fn route_checks_count_every_policy_defaults_rate_limit_names() {
    let mut feed = fixture("catalog/gateway-feed.json");
    feed["rate_limits"]["default"] = json!({ "requests": 1, "window": "1s" });
    feed["rate_limits"]["burst"] = json!({ "requests": 2, "window": "10s" });
    feed["rate_limits"]["standard"] = json!({ "requests": 5, "window": "60s" });
    feed["defaults"]["rate_limit"] = json!(["standard", "burst"]);
    let snapshot = build(feed).unwrap();
    let identity = ByChain::with(&[("acme", org(&["*"]))]);
    let (p, counter) = checks_pipeline(identity, AUTH_FAILURE);
    let key = key_credential();
    let call = || run_checks(&p, &snapshot, &key, &[("GET", "/orgs/acme")]);
    assert_eq!(call().await, Ok(vec![true]));
    let hash = crate::auth::admit::credential_hash(&key_credential());
    let mut buckets = counter.buckets();
    buckets.sort();
    assert_eq!(
        buckets,
        vec![
            format!("burst:credential:{hash}"),
            format!("standard:credential:{hash}")
        ]
    );
    call().await.unwrap();
    // burst is spent: refused, and standard isn't counted.
    assert!(matches!(
        call().await,
        Err(Rejection::RateLimited {
            retry_after: 10,
            ..
        })
    ));
    assert_eq!(counter.count(&format!("standard:credential:{hash}")), 2);
    // `default` is an ordinary name: nothing counts it.
    assert_eq!(counter.count(&format!("default:credential:{hash}")), 0);
}

#[tokio::test]
async fn route_checks_without_identity_are_503() {
    let snapshot = snapshot_with(e10s(), e10s_limits());
    let identity = Arc::new(ByChain {
        down: true,
        ..Default::default()
    });
    let (p, _) = checks_pipeline(identity, AUTH_FAILURE);
    assert_eq!(
        run_checks(&p, &snapshot, &key_credential(), &[("GET", "/orgs/acme")]).await,
        Err(Rejection::Unavailable)
    );
    assert_eq!(
        run_checks(&p, &snapshot, &key_credential(), &[("GET", "/user")]).await,
        Err(Rejection::Unavailable)
    );
    // A call that needs no admit never calls identity, so it still answers.
    assert_eq!(
        run_checks(
            &p,
            &snapshot,
            &key_credential(),
            &[("GET", "/health"), ("GET", "/nowhere")]
        )
        .await,
        Ok(vec![true, false])
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

/// A 401's line says why in `reason`, as the auth-failure metric does; the error
/// fields are for 5xx only (docs/reference/telemetry.md#gateway-authentication-failed-line).
#[test]
fn authentication_failed_line() {
    let lines = crate::telemetry::logged(|| {
        unauthorized(
            AuthError::InvalidToken {
                reason: "token_expired",
            },
            Some(AuthType::Jwt),
            "organization",
        );
        unauthorized(AuthError::MissingAuthorization, None, "user");
    });
    assert_eq!(lines.len(), 2, "{lines:?}");
    for (line, (auth_type, reason, scope)) in lines.iter().zip([
        ("jwt", "token_expired", "organization"),
        ("none", "missing_authorization", "user"),
    ]) {
        assert_eq!(line["level"], "info");
        assert_eq!(line["message"], "authentication failed");
        assert_eq!(line["auth_type"], auth_type);
        assert_eq!(line["reason"], reason);
        assert_eq!(line["scope"], scope);
        assert!(line.get("error_message").is_none(), "{line}");
        assert!(line.get("error_type").is_none(), "{line}");
    }
}

#[tokio::test]
async fn rate_limiting_fails_open_when_valkey_is_unavailable() {
    let snapshot = catalog();
    let read = project_read(&snapshot);
    let h = harness(Ok(answer_for(&read.route, &["project:read"])));
    h.counter.set_down(true);

    // Limited route succeeds without X-RateLimit-* info.
    let passed = h
        .pipeline
        .decide(
            &read,
            "198.51.100.1",
            Ok(key()),
            &mut RequestCredential::default(),
        )
        .await
        .expect("route fails open");
    assert!(passed.rate_limit.is_none());

    // Refusals preserve the underlying 403/401 instead of turning into 503.
    let h_denied = harness(Ok(answer_for(&read.route, &[])));
    h_denied.counter.set_down(true);
    let (res, _) = h_denied.run(&read, Ok(key())).await;
    assert_eq!(res.err(), Some(Rejection::Denied));

    // POST /access/routes also fails open without rate_limit info.
    let snapshot = snapshot_with(e10s(), e10s_limits());
    let identity = Arc::new(ByChain::default());
    let (p, counter) = checks_pipeline(identity, AUTH_FAILURE);
    counter.set_down(true);
    let checked = p
        .check_routes(
            &snapshot.routes,
            &snapshot.default_policies,
            &key_credential(),
            &[Check {
                method: "GET".into(),
                path: "/health".into(),
            }],
            "198.51.100.1",
            None,
        )
        .await
        .expect("route checks fail open");
    assert_eq!(checked.allowed, vec![true]);
    assert!(checked.rate_limit.is_none());
}

#[tokio::test]
async fn a_scoped_personal_key_on_a_user_route_is_403_and_false_in_route_checks() {
    let snapshot = catalog();
    let me = find(&snapshot, "GET", "/user");
    let h = harness(Ok(scoped_personal_user()));
    let (r, cred) = h.run(&me, Ok(key())).await;
    let rejection = r.err().unwrap();
    assert_eq!(rejection, Rejection::ScopedCredential);
    assert_eq!(rejection.status(), 403);
    let body = rejection.body(&me.route);
    assert_eq!(
        (body.code.as_str(), body.message.as_str(), body.details),
        ("FORBIDDEN", "Scoped API keys can't use this route.", None)
    );
    assert!(h.counter.buckets().is_empty());
    assert_eq!(cred.principal.as_deref(), Some("person"));

    let identity = ByChain::with(&[("", scoped_personal_user()), ("acme", org(&["*"]))]);
    let (p, counter) = checks_pipeline(identity, AUTH_FAILURE);
    let got = run_checks(
        &p,
        &snapshot,
        &key_credential(),
        &[("GET", "/user"), ("GET", "/orgs/acme")],
    )
    .await
    .unwrap();
    assert_eq!(got, vec![false, true]);
    assert_eq!(counter.count(IP_BUCKET), 0);
}

#[tokio::test]
async fn jwt_scopes_are_forwarded_to_admit() {
    let snapshot = catalog();
    let read = project_read(&snapshot);
    let h = harness(Ok(answer_for(&read.route, &["project:read"])));
    let scoped_jwt = Presented::Jwt {
        issuer: "https://auth".into(),
        subject: "sub-1".into(),
        name: Some("Dana".into()),
        scopes: Some(vec!["project:read".into()]),
        policy_claims: BTreeMap::from([("amr".into(), vec!["mfa".into()])]),
        kid: Some("kid-1".into()),
    };
    assert!(h.decide(&read, Ok(scoped_jwt)).await.is_ok());
    assert_eq!(
        h.last_request().credential,
        AdmitCredential::Idp {
            issuer: "https://auth".into(),
            subject: "sub-1".into(),
            name: Some("Dana".into()),
            scopes: Some(vec!["project:read".into()]),
            claims: BTreeMap::from([("amr".into(), vec!["mfa".into()])]),
        }
    );
}

#[tokio::test]
async fn org_ip_allowlist_blocks_with_403_and_counts_refusal() {
    let snapshot = catalog();
    let read = project_read(&snapshot);
    let mut ans = answer_for(&read.route, &["project:read"]);
    ans.org_allowed_cidrs = vec!["10.0.0.0/8".into()];
    let h = harness(Ok(ans));
    let (res, _) = h.run(&read, Ok(jwt())).await;
    assert_eq!(res.unwrap_err(), Rejection::NotFound);
    assert_eq!(h.counter.count(IP_BUCKET), 1);

    // Matching CIDR passes.
    let mut ans_ok = answer_for(&read.route, &["project:read"]);
    ans_ok.org_allowed_cidrs = vec!["203.0.113.0/24".into()];
    let h_ok = harness(Ok(ans_ok));
    assert!(h_ok.decide(&read, Ok(jwt())).await.is_ok());
}

#[tokio::test]
async fn rate_limit_tiers_apply_per_root_tier() {
    let limits = json!({
        "evals-caller": { "requests": 100, "window": "1s" },
        "evals-org": {
            "requests": 1,
            "window": "10s",
            "count_at": "organization",
            "tiers": {
                "enterprise": { "requests": 5, "window": "60s" }
            }
        }
    });
    let snapshot = snapshot_with(e10s(), limits);
    let check = find(&snapshot, "POST", "/orgs/acme/projects/web/check");

    // Default (no tier) gets limit 1 / 10s.
    let h = harness(Ok(answer_for(&check.route, &["check"])));
    let passed = h.run(&check, Ok(key())).await.0.unwrap();
    assert_eq!(
        passed.rate_limit,
        Some(RateLimitInfo {
            limit: 1,
            remaining: 0,
            reset_epoch: 10
        })
    );

    // Enterprise tier gets limit 5 / 60s.
    let mut ent = answer_for(&check.route, &["check"]);
    ent.rate_limit_tier = Some("enterprise".into());
    let h_ent = harness(Ok(ent));
    let passed = h_ent.run(&check, Ok(key())).await.0.unwrap();
    assert_eq!(
        passed.rate_limit,
        Some(RateLimitInfo {
            limit: 5,
            remaining: 4,
            reset_epoch: 60
        })
    );

    // Unknown tier falls back to policy default (1 / 10s).
    let mut unknown = answer_for(&check.route, &["check"]);
    unknown.rate_limit_tier = Some("custom".into());
    let h_unk = harness(Ok(unknown));
    let passed = h_unk.run(&check, Ok(key())).await.0.unwrap();
    assert_eq!(
        passed.rate_limit,
        Some(RateLimitInfo {
            limit: 1,
            remaining: 0,
            reset_epoch: 10
        })
    );
}

#[tokio::test]
async fn host_based_route_checks_resolve_against_request_host() {
    let mut feed = fixture("catalog/gateway-feed.json");
    feed["kinds"]["organization"]["host"] = json!("{organization}.api.example.com");
    let snapshot = build(feed).unwrap();
    let identity = ByChain::with(&[
        ("acme", org(&["organization:read"])),
        ("acme/web", project(&["project:read"])),
        ("", user()),
    ]);
    let (p, _) = checks_pipeline(identity, AUTH_FAILURE);
    let checked = p
        .check_routes(
            &snapshot.routes,
            &snapshot.default_policies,
            &jwt_credential(),
            &checks(&[
                ("GET", "/"),
                ("GET", "/projects/web"),
                ("GET", "/user"),
                ("GET", "/orgs/other"),
            ]),
            IP,
            Some("acme.api.example.com"),
        )
        .await
        .unwrap();
    assert_eq!(checked.allowed, vec![true, true, true, false]);
}

#[derive(Default)]
struct FakePdp {
    seen: Mutex<Vec<crate::authorizer::AuthzenRequest>>,
    batch_calls: std::sync::atomic::AtomicUsize,
    allow: Mutex<std::collections::HashSet<(String, String, String)>>,
    down: std::sync::atomic::AtomicBool,
}

impl FakePdp {
    fn grant(&self, action: &str, res_type: &str, res_id: &str) {
        self.allow
            .lock()
            .unwrap()
            .insert((action.into(), res_type.into(), res_id.into()));
    }

    fn set_down(&self, down: bool) {
        self.down.store(down, Ordering::SeqCst);
    }

    fn calls(&self) -> usize {
        self.seen.lock().unwrap().len()
    }
}

impl crate::authorizer::Authorizer for FakePdp {
    async fn evaluate(
        &self,
        _spec: &crate::authorizer::AuthorizerSpec,
        req: &crate::authorizer::AuthzenRequest,
        _request_id: &str,
    ) -> Result<bool, crate::authorizer::AuthorizerUnavailable> {
        self.seen.lock().unwrap().push(req.clone());
        if self.down.load(Ordering::SeqCst) {
            return Err(crate::authorizer::AuthorizerUnavailable("down".into()));
        }
        let key = (
            req.action.name.clone(),
            req.resource.r#type.clone(),
            req.resource.id.clone(),
        );
        Ok(self.allow.lock().unwrap().contains(&key))
    }

    async fn evaluate_batch(
        &self,
        _spec: &crate::authorizer::AuthorizerSpec,
        reqs: &[crate::authorizer::AuthzenRequest],
        _semantic: crate::authorizer::BatchSemantic,
        _request_id: &str,
    ) -> Result<Vec<bool>, crate::authorizer::AuthorizerUnavailable> {
        self.batch_calls.fetch_add(1, Ordering::SeqCst);
        self.seen.lock().unwrap().extend(reqs.iter().cloned());
        if self.down.load(Ordering::SeqCst) {
            return Err(crate::authorizer::AuthorizerUnavailable("down".into()));
        }
        let allow = self.allow.lock().unwrap();
        Ok(reqs
            .iter()
            .map(|r| {
                allow.contains(&(
                    r.action.name.clone(),
                    r.resource.r#type.clone(),
                    r.resource.id.clone(),
                ))
            })
            .collect())
    }
}

fn authorizer_snapshot() -> Snapshot {
    let mut feed = fixture("catalog/gateway-feed.json");
    feed["authorizer"] = json!({
        "url": "http://topaz:8383",
        "timeout": "2s",
        "cache_ttl": "0s",
        "negative_cache_ttl": "0s"
    });
    feed["services"]["docs"] = json!({
        "url": "http://docs:3000",
        "timeouts": { "connect": "2s", "read": "30s" },
        "max_body_bytes": 1048576,
        "routes": [
            {
                "path": "/orgs/{organization}/projects/{project}/docs/{doc_id}",
                "method": "GET",
                "scope": "project",
                "upstream": "/projects/{project.id}/docs/{doc_id}",
                "permissions": ["docs:read"],
                "rate_limit": [],
                "audit": false,
                "allow_deleted": false,
                "resource": { "type": "document", "id": "{doc_id}" }
            },
            {
                "path": "/orgs/{organization}/projects/{project}/docs/{doc_id}",
                "method": "DELETE",
                "scope": "project",
                "upstream": "/projects/{project.id}/docs/{doc_id}",
                "permissions": ["docs:delete"],
                "rate_limit": [],
                "audit": false,
                "allow_deleted": false,
                "resource": { "type": "document", "id": "{doc_id}" }
            },
            {
                "path": "/orgs/{organization}/projects/{project}/summary",
                "method": "GET",
                "scope": "project",
                "upstream": "/projects/{project.id}/summary",
                "permissions": "any_member",
                "rate_limit": [],
                "audit": false,
                "allow_deleted": false
            }
        ]
    });
    build(feed).unwrap()
}

#[tokio::test]
async fn outbound_authzen_all_or_nothing_record_resources_scopes_and_fail_closed() {
    let snapshot = authorizer_snapshot();
    let get_doc = snapshot
        .routes
        .find_route("/orgs/acme/projects/web/docs/doc-42", "GET")
        .unwrap();
    let delete_doc = snapshot
        .routes
        .find_route("/orgs/acme/projects/web/docs/doc-42", "DELETE")
        .unwrap();
    let project_read_route = snapshot
        .routes
        .find_route("/orgs/acme/projects/web", "GET")
        .unwrap();
    let any_member_route = snapshot
        .routes
        .find_route("/orgs/acme/projects/web/summary", "GET")
        .unwrap();

    // 1. All-or-nothing PDP mode: every permission route delegates to the top-level
    // authorizer (even when built-in permissions are empty or `*`). Record-level
    // `resource` overrides resource type/id; routes without `resource` default to kind + node_id.
    let pdp = Arc::new(FakePdp::default());
    pdp.grant("docs:read", "document", "doc-42");
    pdp.grant("project:read", "project", "01nproject");
    let p = Pipeline::with_authorizer(
        AdmitCache::new(
            Arc::new(FakeAdmit::new(Ok(project(&[])))),
            Duration::from_secs(60),
            Duration::from_secs(10),
        ),
        Arc::new(MemCounter::default()),
        AUTH_FAILURE,
        crate::authorizer::AuthorizerCache::new(pdp.clone()),
    );
    let mut cred = RequestCredential::default();
    p.decide_with_authorizer(
        &get_doc,
        IP,
        Ok(jwt()),
        &mut cred,
        snapshot.authorizer.as_ref(),
        "r1",
    )
    .await
    .unwrap();
    assert_eq!(pdp.calls(), 1);
    let sent = pdp.seen.lock().unwrap()[0].clone();
    assert_eq!(sent.subject.r#type, "member");
    assert_eq!(sent.subject.id, MEMBER);
    assert_eq!(sent.subject.properties["principal"], json!("person"));
    assert_eq!(sent.subject.properties["root_id"], json!("01norg"));
    assert_eq!(sent.action.name, "docs:read");
    assert_eq!(sent.resource.r#type, "document");
    assert_eq!(sent.resource.id, "doc-42");

    // Default kind resource (no route.resource) sends { type: "project", id: "01nproject" }.
    p.decide_with_authorizer(
        &project_read_route,
        IP,
        Ok(jwt()),
        &mut RequestCredential::default(),
        snapshot.authorizer.as_ref(),
        "r2",
    )
    .await
    .unwrap();
    assert_eq!(pdp.calls(), 2);
    let sent_proj = pdp.seen.lock().unwrap()[1].clone();
    assert_eq!(sent_proj.resource.r#type, "project");
    assert_eq!(sent_proj.resource.id, "01nproject");

    // 2. `any_member` routes are satisfied by membership at admit and never call the PDP.
    p.decide_with_authorizer(
        &any_member_route,
        IP,
        Ok(jwt()),
        &mut RequestCredential::default(),
        snapshot.authorizer.as_ref(),
        "r3",
    )
    .await
    .unwrap();
    assert_eq!(pdp.calls(), 2);

    // 3. Credential scopes cannot be widened by the PDP.
    let pdp = Arc::new(FakePdp::default());
    let mut scoped_admin = project(&["*"]);
    scoped_admin.credential_scopes = Some(vec!["docs:read".into()]);
    let p_scoped = Pipeline::with_authorizer(
        AdmitCache::new(
            Arc::new(FakeAdmit::new(Ok(scoped_admin))),
            Duration::from_secs(60),
            Duration::from_secs(10),
        ),
        Arc::new(MemCounter::default()),
        AUTH_FAILURE,
        crate::authorizer::AuthorizerCache::new(pdp.clone()),
    );
    pdp.grant("docs:delete", "document", "doc-42");
    let err = p_scoped
        .decide_with_authorizer(
            &delete_doc,
            IP,
            Ok(jwt()),
            &mut RequestCredential::default(),
            snapshot.authorizer.as_ref(),
            "r4",
        )
        .await
        .unwrap_err();
    assert_eq!(err.rejection, Rejection::Denied);
    assert_eq!(pdp.calls(), 0);

    // 4. Fail-closed 503 when PDP is unavailable, without consuming failed-auth IP limiter.
    let pdp = Arc::new(FakePdp::default());
    pdp.set_down(true);
    let counter = Arc::new(MemCounter::default());
    let p_down = Pipeline::with_authorizer(
        AdmitCache::new(
            Arc::new(FakeAdmit::new(Ok(project(&["*"])))),
            Duration::from_secs(60),
            Duration::from_secs(10),
        ),
        counter.clone(),
        AUTH_FAILURE,
        crate::authorizer::AuthorizerCache::new(pdp.clone()),
    );
    let err = p_down
        .decide_with_authorizer(
            &delete_doc,
            IP,
            Ok(jwt()),
            &mut RequestCredential::default(),
            snapshot.authorizer.as_ref(),
            "r5",
        )
        .await
        .unwrap_err();
    assert_eq!(err.rejection, Rejection::Unavailable);
    assert_eq!(counter.count(IP_BUCKET), 0);

    // 5. POST /access/routes batches multiple external checks into ONE evaluate_batch call
    // and returns the recognized API key_id.
    let pdp = Arc::new(FakePdp::default());
    pdp.grant("docs:read", "document", "doc-1");
    pdp.grant("docs:delete", "document", "doc-1");
    let p_checks = Pipeline::with_authorizer(
        AdmitCache::new(
            ByChain::with(&[("acme/web", project(&[]))]),
            Duration::from_secs(60),
            Duration::from_secs(10),
        ),
        Arc::new(MemCounter::default()),
        AUTH_FAILURE,
        crate::authorizer::AuthorizerCache::new(pdp.clone()),
    );
    let checked = p_checks
        .check_routes_with_authorizer(
            &snapshot.routes,
            &snapshot.default_policies,
            snapshot.authorizer.as_ref(),
            &key_credential(),
            &checks(&[
                ("GET", "/orgs/acme/projects/web/docs/doc-1"),
                ("GET", "/orgs/acme/projects/web/docs/doc-2"),
                ("DELETE", "/orgs/acme/projects/web/docs/doc-1"),
                ("DELETE", "/orgs/acme/projects/web/docs/doc-2"),
            ]),
            IP,
            None,
            "r6",
        )
        .await
        .unwrap();
    assert_eq!(checked.allowed, vec![true, false, true, false]);
    assert_eq!(checked.key_id.as_deref(), Some(KEY_ID));
    assert_eq!(pdp.batch_calls.load(Ordering::SeqCst), 1);

    // 6. Invalid API key on POST /access/routes does NOT write a credential rate-limit bucket.
    let counter = Arc::new(MemCounter::default());
    let p_invalid = Pipeline::with_authorizer(
        AdmitCache::new(
            ByChain::with(&[("acme/web", invalid())]),
            Duration::from_secs(60),
            Duration::from_secs(10),
        ),
        counter.clone(),
        AUTH_FAILURE,
        crate::authorizer::AuthorizerCache::new(pdp.clone()),
    );
    let err = p_invalid
        .check_routes_with_authorizer(
            &snapshot.routes,
            &snapshot.default_policies,
            snapshot.authorizer.as_ref(),
            &AdmitCredential::Credential("bogus-attacker-token".into()),
            &checks(&[("GET", "/orgs/acme/projects/web/docs/doc-1")]),
            IP,
            None,
            "r7",
        )
        .await
        .unwrap_err();
    assert!(matches!(
        err,
        Rejection::Unauthorized(AuthError::InvalidCredential)
    ));
    assert_eq!(counter.buckets(), vec![IP_BUCKET]);
}
