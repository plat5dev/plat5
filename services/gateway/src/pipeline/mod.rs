//! How the gateway decides a request (docs/concepts/access.md#how-a-request-is-decided),
//! apart from the proxy around it: authenticate and admit, the permission
//! check, rate limit, and the upstream fill. The proxy writes the audit intent after
//! the fill, and turns a [`Rejection`] into a response.

mod checks;
mod permissions;

use std::collections::BTreeMap;
use std::future::Future;

use serde_json::{json, Value};
use tracing::{info, warn};

use crate::auth::admit::{
    Admit, AdmitAnswer, AdmitCache, AdmitCredential, AdmitRequest, ChainRef, NodeRef,
};
use crate::auth::{AuthError, AuthType, Presented};
use crate::authorizer::{
    Authorizer, AuthorizerCache, AuthorizerMode, AuthorizerSpec, AuthzenAction, AuthzenRequest,
    AuthzenResource, AuthzenSubject, NoAuthorizer,
};
use crate::error::{ApiError, ErrorKind};
use crate::kinds::id_header;
use crate::metrics;
use crate::rate_limit::{Bucket, RateLimitInfo, RateLimiter, Unavailable, Verdict};
use crate::route_map::{BadParam, Matched, Required, Route, Scope, Values};

pub use checks::{parse_checks, BadField, Check, Checked, MAX_CHECKS, MAX_CHECKS_BODY};
pub use permissions::permitted;

pub const MEMBER_ID_HEADER: &str = "X-Plat5-Member-Id";
pub const USER_ID_HEADER: &str = "X-Plat5-User-Id";
pub const PRINCIPAL_HEADER: &str = "X-Plat5-Principal";

/// Admit's `principal` for a service account.
const SERVICE_ACCOUNT: &str = "service_account";

/// The failed-auth limiter's bucket. `@` is in no policy name, so no policy shares it.
const AUTH_FAILURE_BUCKET: &str = "@auth-failure";

/// Fixed-window counters, checked together. Valkey in the gateway; in memory in tests.
pub trait Counter: Send + Sync {
    fn check(
        &self,
        buckets: &[Bucket],
    ) -> impl Future<Output = Result<Verdict, Unavailable>> + Send;
}

impl Counter for RateLimiter {
    async fn check(&self, buckets: &[Bucket]) -> Result<Verdict, Unavailable> {
        RateLimiter::check(self, buckets).await
    }
}

/// The failed-auth IP limiter's requests per window. `0` requests = off.
#[derive(Clone, Copy, Debug)]
pub struct Limit {
    pub requests: u64,
    pub window_seconds: u64,
}

/// The caller, once admitted on a kind or user route.
#[derive(Clone, Debug, PartialEq, Eq)]
pub struct Caller {
    /// Credential kind. `None` for anonymous callers on `allow_public` routes.
    pub auth_type: Option<AuthType>,
    /// The key's id, from admit. None for a JWT.
    pub key_id: Option<String>,
    /// A JWT's key id, for the request span.
    pub kid: Option<String>,
    /// `person`, `service_account`, or `anonymous`.
    pub principal: String,
    /// The route's chain, root first. Empty on a user route.
    pub nodes: Vec<NodeRef>,
    /// The caller in this tree, on a kind route.
    pub member_id: Option<String>,
    /// The person, strictly on a user route (`X-Plat5-User-Id` & user audit events).
    pub user_id: Option<String>,
    /// Authenticated non-member's user id on an `allow_public` tree route: used
    /// only for outbound AuthZEN `subject: { type: "user", id }`, never forwarded
    /// upstream in headers or audit.
    pub authzen_user_id: Option<String>,
    /// The caller's permissions on the route's node. Log only: never sent on.
    pub permissions: Vec<String>,
    /// Optional explicit credential scopes when a key or JWT is scope-narrowed.
    pub credential_scopes: Option<Vec<String>>,
    /// Optional opaque rate-limit tier from the root node.
    pub rate_limit_tier: Option<String>,
}

impl Caller {
    pub fn root_id(&self) -> Option<&str> {
        self.nodes.first().map(|n| n.id.as_str())
    }

    pub fn node_id(&self) -> Option<&str> {
        self.nodes.last().map(|n| n.id.as_str())
    }
}

#[derive(Clone, Debug, PartialEq, Eq)]
#[allow(clippy::large_enum_variant)]
pub enum Admitted {
    Public,
    Caller(Caller),
}

/// Past the rate limit: who, and the tightest policy's numbers for the response.
#[derive(Debug)]
pub struct Passed {
    pub admitted: Admitted,
    pub rate_limit: Option<RateLimitInfo>,
}

/// A request the pipeline answers instead of forwarding, and the caller when it
/// was admitted: a permission **403** is logged and audited in their tree.
#[derive(Debug)]
pub struct Refused {
    pub rejection: Rejection,
    pub caller: Option<Box<Caller>>,
}

impl From<Rejection> for Refused {
    fn from(rejection: Rejection) -> Self {
        Refused {
            rejection,
            caller: None,
        }
    }
}

/// Who presented the request, for the request log, as far as the gateway got.
/// Never a secret.
#[derive(Debug, Default, Clone, PartialEq, Eq)]
pub struct RequestCredential {
    pub auth_type: Option<AuthType>,
    /// The key's id, once admit recognized the key.
    pub key_id: Option<String>,
    pub principal: Option<String>,
    pub root_id: Option<String>,
    pub node_id: Option<String>,
    pub member_id: Option<String>,
    pub user_id: Option<String>,
}

/// Why the gateway answers instead of forwarding.
#[derive(Debug, PartialEq)]
pub enum Rejection {
    /// **401**. `details` is a bad JWT's reason.
    Unauthorized(AuthError),
    /// **403** at admit: no such node, or no member in its tree.
    NotFound,
    /// **403** at the permission check: no route permission held.
    Denied,
    /// **403** before the rate limit: a service account on a user route.
    ServiceAccount,
    /// **403** before the rate limit: a scoped or root-restricted personal API key on a user route.
    ScopedCredential,
    /// **429**. `info` is set on an admitted route, None on the failed-auth limiter.
    RateLimited {
        retry_after: u64,
        info: Option<RateLimitInfo>,
    },
    /// **400**: a route param that would change the upstream's path.
    BadPathParam,
    /// **503**: identity, JWKS or Valkey.
    Unavailable,
}

impl Rejection {
    pub fn status(&self) -> u16 {
        match self {
            Rejection::Unauthorized(_) => 401,
            Rejection::NotFound
            | Rejection::Denied
            | Rejection::ServiceAccount
            | Rejection::ScopedCredential => 403,
            Rejection::RateLimited { .. } => 429,
            Rejection::BadPathParam => 400,
            Rejection::Unavailable => 503,
        }
    }

    /// The body. Both 403s are the same, so neither says whether the node exists.
    pub fn body(&self, route: &Route) -> ApiError {
        match self {
            Rejection::Unauthorized(err) => ApiError::unauthorized_for(&route.scope, err.details()),
            Rejection::NotFound | Rejection::Denied => ApiError::forbidden(&route.required),
            Rejection::ServiceAccount => ApiError::service_account_forbidden(),
            Rejection::ScopedCredential => ApiError::scoped_credential_forbidden(),
            Rejection::RateLimited { retry_after, .. } => ApiError::rate_limited(*retry_after),
            Rejection::BadPathParam => ApiError::invalid_request(),
            Rejection::Unavailable => ApiError::service_unavailable(),
        }
    }
}

/// The upstream request: the filled path and the caller's headers.
#[derive(Debug, PartialEq, Eq)]
pub struct Forward {
    pub path: String,
    pub headers: Vec<(String, String)>,
}

/// Outcome of the shared admit step before request-level vs route-check handling.
pub(super) enum AdmitOutcome {
    Admitted(Box<Caller>),
    /// Credential itself was invalid or rejected by key IP allowlist (**401**).
    InvalidCredential {
        auth_type: Option<AuthType>,
        key_id: Option<String>,
        ip_rejected: bool,
    },
    /// Valid service account on a user route (**403**, not counted by auth-failure limiter).
    ServiceAccount {
        principal: Option<String>,
    },
    /// Valid narrowed personal key on a user route (**403**, not counted by auth-failure limiter).
    ScopedCredential {
        principal: Option<String>,
    },
    /// Org IP allowlist, missing node, or caller not admitted to this tree (**403** / **401** anon).
    NotAdmitted {
        anonymous: bool,
        org_ip_rejected: bool,
    },
    /// Identity didn't answer (**503**).
    Unavailable,
}

/// Outcome of pre-PDP authorization shared between `decide` and `POST /access/routes`.
pub(super) enum AuthzDecision {
    Allow,
    Deny,
    External {
        authorizer: String,
        requests: Vec<AuthzenRequest>,
    },
}

pub struct Pipeline<A, C, P = NoAuthorizer> {
    admit: AdmitCache<A>,
    counter: C,
    auth_failure: Limit,
    authorizer: AuthorizerCache<P>,
}

impl<A: Admit, C: Counter> Pipeline<A, C, NoAuthorizer> {
    pub fn new(admit: AdmitCache<A>, counter: C, auth_failure: Limit) -> Self {
        Self {
            admit,
            counter,
            auth_failure,
            authorizer: AuthorizerCache::new(NoAuthorizer),
        }
    }
}

impl<A: Admit, C: Counter, P: Authorizer> Pipeline<A, C, P> {
    pub fn with_authorizer(
        admit: AdmitCache<A>,
        counter: C,
        auth_failure: Limit,
        authorizer: AuthorizerCache<P>,
    ) -> Self {
        Self {
            admit,
            counter,
            auth_failure,
            authorizer,
        }
    }

    /// Authenticate and admit, the permission check, then rate limit.
    /// `presented` is what [`crate::auth::Authenticator::present`] found. `cred`
    /// is filled as far as it gets.
    pub async fn decide(
        &self,
        matched: &Matched,
        ip: &str,
        presented: Result<Presented, AuthError>,
        cred: &mut RequestCredential,
    ) -> Result<Passed, Refused> {
        self.decide_with_authorizers(matched, ip, presented, cred, &BTreeMap::new(), "")
            .await
    }

    pub async fn decide_with_authorizers(
        &self,
        matched: &Matched,
        ip: &str,
        presented: Result<Presented, AuthError>,
        cred: &mut RequestCredential,
        authorizers: &BTreeMap<String, AuthorizerSpec>,
        request_id: &str,
    ) -> Result<Passed, Refused> {
        let route = &matched.route;
        let admitted = match &route.scope {
            Scope::Public => Admitted::Public,
            _ => Admitted::Caller(self.admit(matched, ip, presented, cred).await?),
        };
        let caller = || match &admitted {
            Admitted::Caller(c) => Some(Box::new(c.clone())),
            Admitted::Public => None,
        };
        // Before the rate limit, so only callers the route lets through spend
        // its budgets. Counted with admit's 403, so the two stay one answer.
        // Anonymous callers on `allow_public` routes get a uniform 401 so
        // missing vs. non-public nodes never differ.
        if let Err(err) = self
            .authorize_route(matched, &admitted, authorizers, ip, request_id)
            .await
        {
            match err {
                Rejection::Unavailable | Rejection::BadPathParam => {
                    return Err(Refused {
                        rejection: err,
                        caller: caller(),
                    });
                }
                denied => {
                    let base = match &admitted {
                        Admitted::Caller(c) if c.auth_type.is_none() => {
                            unauthorized(AuthError::MissingAuthorization, None, route.scope.name())
                        }
                        _ => denied,
                    };
                    let rejection = self.count_refusal(ip).await.unwrap_or(base);
                    return Err(Refused {
                        rejection,
                        caller: caller(),
                    });
                }
            }
        }
        let rate_limit = self
            .limit(route, ip, &admitted)
            .await
            .map_err(|rejection| Refused {
                rejection,
                caller: caller(),
            })?;
        Ok(Passed {
            admitted,
            rate_limit,
        })
    }

    async fn authorize_route(
        &self,
        matched: &Matched,
        admitted: &Admitted,
        authorizers: &BTreeMap<String, AuthorizerSpec>,
        ip: &str,
        request_id: &str,
    ) -> Result<(), Rejection> {
        let Admitted::Caller(caller) = admitted else {
            return Ok(());
        };
        match prepare_authorization(matched, caller, ip, request_id)? {
            AuthzDecision::Allow => Ok(()),
            AuthzDecision::Deny => Err(Rejection::Denied),
            AuthzDecision::External {
                authorizer,
                requests,
            } => {
                let spec = authorizers.get(&authorizer).ok_or(Rejection::Unavailable)?;
                let allowed = self
                    .authorizer
                    .evaluate_any(spec, &requests, request_id)
                    .await
                    .map_err(|_| Rejection::Unavailable)?;
                if allowed {
                    Ok(())
                } else {
                    Err(Rejection::Denied)
                }
            }
        }
    }

    /// Shared admit evaluation for both normal requests and `POST /access/routes`.
    /// Also returns `key_id` whenever identity recognized an API key, for request logs.
    pub(super) async fn admit_credential(
        &self,
        matched: &Matched,
        credential: AdmitCredential,
        kid: Option<String>,
        ip: &str,
    ) -> (Option<String>, AdmitOutcome) {
        let route = &matched.route;
        let auth_type = credential.auth_type();
        let anonymous = matches!(credential, AdmitCredential::Anonymous(_));
        let external_can_grant = route
            .authorizer
            .as_ref()
            .is_some_and(|a| matches!(a.mode, AuthorizerMode::Replace | AuthorizerMode::Or));
        let req = admit_request(matched, credential);
        let Ok(answer) = self.admit.get(&req).await else {
            return (None, AdmitOutcome::Unavailable);
        };
        let key_id = answer.key_id.clone();
        if !anonymous && (!answer.valid || !answer.allows_ip(ip)) {
            return (
                key_id.clone(),
                AdmitOutcome::InvalidCredential {
                    auth_type,
                    key_id,
                    ip_rejected: answer.valid,
                },
            );
        }
        if !anonymous && service_account_on_user_route(&req, &answer) {
            return (
                key_id,
                AdmitOutcome::ServiceAccount {
                    principal: answer.principal.clone(),
                },
            );
        }
        if !anonymous && scoped_key_on_user_route(&req, &answer) {
            return (
                key_id,
                AdmitOutcome::ScopedCredential {
                    principal: answer.principal.clone(),
                },
            );
        }
        if !answer.valid {
            return (
                key_id,
                AdmitOutcome::NotAdmitted {
                    anonymous,
                    org_ip_rejected: false,
                },
            );
        }
        if !answer.allows_org_ip(ip) {
            return (
                key_id,
                AdmitOutcome::NotAdmitted {
                    anonymous,
                    org_ip_rejected: true,
                },
            );
        }
        if answer.refused_with_external(&req, external_can_grant) {
            return (
                key_id,
                AdmitOutcome::NotAdmitted {
                    anonymous,
                    org_ip_rejected: false,
                },
            );
        }
        (
            key_id,
            AdmitOutcome::Admitted(Box::new(caller_from_answer(&req, &answer, auth_type, kid))),
        )
    }

    async fn admit(
        &self,
        matched: &Matched,
        ip: &str,
        presented: Result<Presented, AuthError>,
        cred: &mut RequestCredential,
    ) -> Result<Caller, Rejection> {
        let route = &matched.route;
        let (credential, kid) = match presented {
            Err(AuthError::MissingAuthorization) if route.allow_public => {
                (AdmitCredential::Anonymous(true), None)
            }
            // Refused here, without identity: not counted.
            Err(err) => {
                let auth_type = err.auth_type();
                cred.auth_type = auth_type;
                return Err(unauthorized(err, auth_type, route.scope.name()));
            }
            Ok(p) => {
                cred.auth_type = Some(p.auth_type());
                p.into_admit()
            }
        };
        let (key_id, outcome) = self.admit_credential(matched, credential, kid, ip).await;
        if key_id.is_some() {
            cred.key_id = key_id;
        }
        match outcome {
            AdmitOutcome::Admitted(caller) => {
                let caller = *caller;
                cred.principal = Some(caller.principal.clone());
                cred.member_id = caller.member_id.clone();
                cred.user_id = caller.user_id.clone();
                cred.root_id = caller.root_id().map(str::to_string);
                cred.node_id = caller.node_id().map(str::to_string);
                Ok(caller)
            }
            AdmitOutcome::InvalidCredential {
                auth_type,
                key_id,
                ip_rejected,
            } => {
                if ip_rejected {
                    info!(
                        key_id = key_id.as_deref().unwrap_or(""),
                        client_ip = ip,
                        "api key rejected by ip allowlist"
                    );
                }
                let rejection =
                    unauthorized(AuthError::InvalidCredential, auth_type, route.scope.name());
                Err(self.count_refusal(ip).await.unwrap_or(rejection))
            }
            AdmitOutcome::ServiceAccount { principal } => {
                cred.principal = principal;
                Err(Rejection::ServiceAccount)
            }
            AdmitOutcome::ScopedCredential { principal } => {
                cred.principal = principal;
                Err(Rejection::ScopedCredential)
            }
            AdmitOutcome::NotAdmitted {
                anonymous,
                org_ip_rejected,
            } => {
                if org_ip_rejected {
                    info!(
                        client_ip = ip,
                        "caller rejected by root security policy ip allowlist"
                    );
                }
                let rejection = if anonymous {
                    unauthorized(AuthError::MissingAuthorization, None, route.scope.name())
                } else {
                    Rejection::NotFound
                };
                Err(self.count_refusal(ip).await.unwrap_or(rejection))
            }
            AdmitOutcome::Unavailable => Err(Rejection::Unavailable),
        }
    }

    /// The failed-auth IP limiter, on what admit refused and on the permission
    /// **403**. Some(rejection) when it answers instead. Fails open when Valkey
    /// is unavailable so the caller receives the underlying 401/403.
    async fn count_refusal(&self, ip: &str) -> Option<Rejection> {
        if self.auth_failure.requests == 0 {
            return None;
        }
        let bucket = Bucket {
            key: format!("{AUTH_FAILURE_BUCKET}:{ip}"),
            requests: self.auth_failure.requests,
            window_seconds: self.auth_failure.window_seconds,
        };
        match self.counter.check(&[bucket]).await {
            Ok(Verdict::Allowed(_)) => None,
            Ok(Verdict::Exceeded { retry_after, .. }) => Some(Rejection::RateLimited {
                retry_after,
                info: None,
            }),
            Err(Unavailable) => {
                metrics::record_rate_limit_fail_open("auth_failure");
                None
            }
        }
    }

    /// Every policy on the route, checked at once. Ok carries the tightest one's
    /// numbers, or None when no policy applies or Valkey failed open.
    async fn limit(
        &self,
        route: &Route,
        ip: &str,
        admitted: &Admitted,
    ) -> Result<Option<RateLimitInfo>, Rejection> {
        let buckets = buckets(route, ip, admitted);
        self.check_buckets(&buckets, "route").await
    }

    pub(super) async fn check_buckets(
        &self,
        buckets: &[Bucket],
        fail_open_label: &str,
    ) -> Result<Option<RateLimitInfo>, Rejection> {
        match self.counter.check(buckets).await {
            Ok(Verdict::Allowed(infos)) => Ok(tightest(infos)),
            Ok(Verdict::Exceeded { retry_after, infos }) => Err(Rejection::RateLimited {
                retry_after,
                info: tightest(infos),
            }),
            Err(Unavailable) => {
                metrics::record_rate_limit_fail_open(fail_open_label);
                Ok(None)
            }
        }
    }
}

fn caller_from_answer(
    req: &AdmitRequest,
    answer: &AdmitAnswer,
    auth_type: Option<AuthType>,
    kid: Option<String>,
) -> Caller {
    let is_tree = !req.chain.is_empty();
    let member_id = answer.member_id.clone().filter(|_| is_tree);
    let user_id = answer.user_id.clone().filter(|_| !is_tree);
    let authzen_user_id = if is_tree && member_id.is_none() {
        answer.user_id.clone()
    } else {
        None
    };
    let principal = answer.principal.clone().unwrap_or_else(|| {
        if auth_type.is_none() {
            "anonymous".to_string()
        } else {
            String::new()
        }
    });
    Caller {
        auth_type,
        key_id: answer.key_id.clone(),
        kid,
        principal,
        nodes: answer.nodes.clone(),
        member_id,
        user_id,
        authzen_user_id,
        permissions: answer.permissions.clone(),
        credential_scopes: answer.credential_scopes.clone(),
        rate_limit_tier: answer.rate_limit_tier.clone(),
    }
}

/// Evaluates built-in RBAC and prepares any required outbound AuthZEN requests,
/// shared by both request execution and `POST /access/routes`.
pub(super) fn prepare_authorization(
    matched: &Matched,
    caller: &Caller,
    ip: &str,
    request_id: &str,
) -> Result<AuthzDecision, Rejection> {
    let route = &matched.route;
    let builtin_ok = permitted(
        &route.required,
        &caller.permissions,
        caller.member_id.is_some(),
    );
    let Some(ra) = route.authorizer.as_ref() else {
        return Ok(if builtin_ok {
            AuthzDecision::Allow
        } else {
            AuthzDecision::Deny
        });
    };
    match ra.mode {
        AuthorizerMode::And if !builtin_ok => return Ok(AuthzDecision::Deny),
        AuthorizerMode::Or if builtin_ok => return Ok(AuthzDecision::Allow),
        _ => {}
    }
    let actions = effective_actions(
        route,
        ra.action.as_deref(),
        caller.credential_scopes.as_deref(),
    );
    if actions.is_empty() {
        return Ok(AuthzDecision::Deny);
    }
    let requests = build_authzen_requests(matched, caller, ra, &actions, ip, request_id)?;
    Ok(AuthzDecision::External {
        authorizer: ra.name.clone(),
        requests,
    })
}

/// A valid service account's answer on a user route (an empty chain): user
/// routes are a person's own, so the gateway refuses it with a **403**.
fn service_account_on_user_route(req: &AdmitRequest, answer: &AdmitAnswer) -> bool {
    answer.valid && req.chain.is_empty() && answer.principal.as_deref() == Some(SERVICE_ACCOUNT)
}

/// A narrowed (scoped or root-restricted) personal API key on a user route:
/// identity returns `user_id: null` so it cannot manage user-level resources.
fn scoped_key_on_user_route(req: &AdmitRequest, answer: &AdmitAnswer) -> bool {
    answer.valid
        && req.chain.is_empty()
        && answer.user_id.is_none()
        && answer.principal.as_deref() != Some(SERVICE_ACCOUNT)
}

/// Admit's request for a matched route: the credential, the chain refs as
/// written in the path, and the route's `allow_deleted` / `allow_public`.
fn admit_request(matched: &Matched, credential: AdmitCredential) -> AdmitRequest {
    let route = &matched.route;
    AdmitRequest {
        credential,
        chain: route
            .chain
            .iter()
            .zip(&matched.refs)
            .map(|(kind, reference)| ChainRef {
                kind: kind.clone(),
                reference: reference.clone(),
            })
            .collect(),
        allow_deleted: route.allow_deleted,
        allow_public: route.allow_public,
    }
}

impl Presented {
    /// The credential as admit takes it, and a JWT's key id for the span.
    pub fn into_admit(self) -> (AdmitCredential, Option<String>) {
        match self {
            Presented::Key(key) => (AdmitCredential::Credential(key), None),
            Presented::Jwt {
                issuer,
                subject,
                name,
                scopes,
                policy_claims,
                kid,
            } => (
                AdmitCredential::Idp {
                    issuer,
                    subject,
                    name,
                    scopes,
                    claims: policy_claims,
                },
                kid,
            ),
        }
    }
}

/// A 401: logged and metered. JWKS not loaded is the gateway's own trouble, **503**.
/// `scope` is the route's scope name, for the log.
fn unauthorized(err: AuthError, auth_type: Option<AuthType>, scope: &str) -> Rejection {
    if !err.is_client_error() {
        warn!(
            error_type = ErrorKind::Network.as_str(),
            error_message = err.as_str(),
            "authentication infrastructure unavailable"
        );
        return Rejection::Unavailable;
    }
    let auth_type = auth_type.map_or("none", AuthType::as_str);
    metrics::record_auth_failure(auth_type, err.as_str());
    info!(
        auth_type,
        reason = err.as_str(),
        scope,
        "authentication failed"
    );
    Rejection::Unauthorized(err)
}

/// The policy with the fewest requests left; on a tie, the one that resets last.
fn tightest(infos: Vec<RateLimitInfo>) -> Option<RateLimitInfo> {
    infos.into_iter().min_by(|a, b| {
        a.remaining
            .cmp(&b.remaining)
            .then_with(|| b.reset_epoch.cmp(&a.reset_epoch))
    })
}

/// Each policy's bucket (docs/reference/config/rate-limits.md#buckets): per caller
/// at the route's node, at the `count_at` node, per user, or per client IP.
pub fn buckets(route: &Route, ip: &str, admitted: &Admitted) -> Vec<Bucket> {
    let tier = match admitted {
        Admitted::Caller(c) => c.rate_limit_tier.as_deref(),
        Admitted::Public => None,
    };
    route
        .policies
        .iter()
        .map(|policy| {
            let at = match (admitted, policy.count_at) {
                (Admitted::Public, _) => format!("ip:{ip}"),
                (Admitted::Caller(c), Some(i)) => c.nodes[i].id.clone(),
                (Admitted::Caller(c), None) => match (&c.member_id, &c.user_id) {
                    (Some(member), _) => {
                        format!("{}:{member}", c.node_id().unwrap_or_default())
                    }
                    (None, Some(user)) => format!("user:{user}"),
                    (None, None) => format!("{}:ip:{ip}", c.node_id().unwrap_or_default()),
                },
            };
            let (requests, window_seconds) = policy.effective(tier);
            Bucket {
                key: format!("{}:{at}", policy.name),
                requests,
                window_seconds,
            }
        })
        .collect()
}

/// The permission check, on a kind route (without external authorizers).
pub fn authorize(route: &Route, admitted: &Admitted) -> Result<(), Rejection> {
    match admitted {
        Admitted::Caller(c)
            if !permitted(&route.required, &c.permissions, c.member_id.is_some()) =>
        {
            Err(Rejection::Denied)
        }
        _ => Ok(()),
    }
}

/// Fill: the upstream path and the caller's headers. No headers on a public route.
pub fn forward(matched: &Matched, admitted: &Admitted) -> Result<Forward, Rejection> {
    let route = &matched.route;
    let caller = match admitted {
        Admitted::Public => None,
        Admitted::Caller(c) => Some(c),
    };
    let nodes: Vec<&str> = caller
        .map(|c| c.nodes.iter().map(|n| n.id.as_str()).collect())
        .unwrap_or_default();
    let path = route
        .upstream
        .fill(Values {
            nodes: &nodes,
            member_id: caller.and_then(|c| c.member_id.as_deref()),
            user_id: caller.and_then(|c| c.user_id.as_deref()),
            params: &matched.params,
        })
        .map_err(|BadParam| Rejection::BadPathParam)?;
    let mut headers = Vec::new();
    if let Some(c) = caller {
        for node in &c.nodes {
            headers.push((id_header(&node.kind), node.id.clone()));
        }
        if let Some(member) = &c.member_id {
            headers.push((MEMBER_ID_HEADER.to_string(), member.clone()));
        }
        if let Some(user) = &c.user_id {
            headers.push((USER_ID_HEADER.to_string(), user.clone()));
        }
        headers.push((PRINCIPAL_HEADER.to_string(), c.principal.clone()));
    }
    Ok(Forward { path, headers })
}

/// Actions to evaluate against an external authorizer, intersected with any
/// explicit credential scopes so a narrowed key or JWT can never be widened.
fn effective_actions(
    route: &Route,
    override_action: Option<&str>,
    credential_scopes: Option<&[String]>,
) -> Vec<String> {
    let actions: Vec<String> = match override_action {
        Some(a) => vec![a.to_string()],
        None => match &route.required {
            Required::AnyOf(list) => list.clone(),
            Required::Nothing | Required::AnyMember => Vec::new(),
        },
    };
    let Some(scopes) = credential_scopes else {
        return actions;
    };
    if scopes.iter().any(|s| s == "*") {
        return actions;
    }
    actions
        .into_iter()
        .filter(|a| scopes.iter().any(|s| s == a))
        .collect()
}

fn build_authzen_requests(
    matched: &Matched,
    caller: &Caller,
    ra: &crate::authorizer::RouteAuthorizer,
    actions: &[String],
    ip: &str,
    request_id: &str,
) -> Result<Vec<AuthzenRequest>, Rejection> {
    let route = &matched.route;
    let nodes_slice: Vec<&str> = caller.nodes.iter().map(|n| n.id.as_str()).collect();
    let (res_type, res_id) = match &ra.resource {
        Some(res) => {
            let filled = res
                .id
                .fill(Values {
                    nodes: &nodes_slice,
                    member_id: caller.member_id.as_deref(),
                    user_id: caller.user_id.as_deref(),
                    params: &matched.params,
                })
                .map_err(|BadParam| Rejection::BadPathParam)?;
            (res.r#type.clone(), filled)
        }
        None => (
            route.scope.name().to_string(),
            caller.node_id().unwrap_or_default().to_string(),
        ),
    };

    let (subj_type, subj_id) = if let Some(member_id) = &caller.member_id {
        ("member".to_string(), member_id.clone())
    } else if let Some(user_id) = caller.authzen_user_id.as_ref().or(caller.user_id.as_ref()) {
        ("user".to_string(), user_id.clone())
    } else {
        ("anonymous".to_string(), String::new())
    };
    let mut subj_props = BTreeMap::new();
    if !caller.principal.is_empty() {
        subj_props.insert("principal".into(), json!(caller.principal));
    }
    if let Some(root_id) = caller.root_id() {
        subj_props.insert("root_id".into(), json!(root_id));
    }
    let subject = AuthzenSubject {
        r#type: subj_type,
        id: subj_id,
        properties: subj_props,
    };

    let mut res_props = BTreeMap::new();
    if let Some(root_id) = caller.root_id() {
        res_props.insert("root_id".into(), json!(root_id));
    }
    for n in &caller.nodes {
        res_props.insert(format!("{}_id", n.kind), json!(n.id));
    }
    res_props.insert(
        "nodes".into(),
        Value::Array(
            caller
                .nodes
                .iter()
                .map(|n| json!({ "kind": n.kind, "id": n.id }))
                .collect(),
        ),
    );
    let resource = AuthzenResource {
        r#type: res_type,
        id: res_id,
        properties: res_props,
    };

    let mut context = BTreeMap::new();
    if !ip.is_empty() {
        context.insert("client_ip".into(), json!(ip));
    }
    if !request_id.is_empty() {
        context.insert("request_id".into(), json!(request_id));
    }
    if !matched.params.is_empty() {
        context.insert("params".into(), json!(matched.params));
    }

    let mut action_props = BTreeMap::new();
    action_props.insert("method".into(), json!(route.method));
    action_props.insert("route".into(), json!(route.path));

    Ok(actions
        .iter()
        .map(|name| AuthzenRequest {
            subject: subject.clone(),
            action: AuthzenAction {
                name: name.clone(),
                properties: action_props.clone(),
            },
            resource: resource.clone(),
            context: context.clone(),
        })
        .collect())
}

/// The proxy's pipeline: identity over HTTP, Valkey counters, outbound AuthZEN PDP client.
pub type GatewayPipeline =
    Pipeline<crate::auth::admit::HttpAdmit, RateLimiter, crate::authorizer::HttpAuthorizer>;

#[cfg(test)]
mod tests;
