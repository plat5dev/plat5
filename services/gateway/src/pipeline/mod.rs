//! How the gateway decides a request (docs/concepts/access.md#how-a-request-is-decided),
//! apart from the proxy around it: authenticate and admit, rate limit, the
//! permission check, and the upstream fill. The proxy writes the audit intent after
//! the fill, and turns a [`Rejection`] into a response.

mod checks;
mod fill;
mod permissions;

use std::future::Future;

use tracing::{info, warn};

use crate::auth::admit::{
    Admit, AdmitAnswer, AdmitCache, AdmitCredential, AdmitRequest, ChainRef, NodeRef,
};
use crate::auth::{AuthError, AuthType, Presented};
use crate::error::{ApiError, ErrorKind};
use crate::kinds::id_header;
use crate::metrics;
use crate::rate_limit::{Bucket, RateLimitInfo, RateLimiter, Unavailable, Verdict};
use crate::route_map::{Matched, Route, Scope};

pub use checks::{
    parse_checks, BadField, Check, CheckRefused, Checked, MAX_CHECKS, MAX_CHECKS_BODY,
};
pub use fill::{path_and_query, BadParam, Upstream, Values};
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
    pub auth_type: AuthType,
    /// The key's id, from admit. None for a JWT.
    pub key_id: Option<String>,
    /// A JWT's key id, for the request span.
    pub kid: Option<String>,
    /// `person` or `service_account`.
    pub principal: String,
    /// The route's chain, root first. Empty on a user route.
    pub nodes: Vec<NodeRef>,
    /// The caller in this tree, on a kind route.
    pub member_id: Option<String>,
    /// The person, on a user route.
    pub user_id: Option<String>,
    /// The caller's permissions on the route's node. Log only: never sent on.
    pub permissions: Vec<String>,
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
    /// **403** before the rate limit: no such node, or no member in its tree.
    NotFound,
    /// **403** after the rate limit: no route permission held.
    Denied,
    /// **403** before the rate limit: a service account on a user route.
    ServiceAccount,
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
            Rejection::NotFound | Rejection::Denied | Rejection::ServiceAccount => 403,
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

pub struct Pipeline<A, C> {
    admit: AdmitCache<A>,
    counter: C,
    auth_failure: Limit,
}

impl<A: Admit, C: Counter> Pipeline<A, C> {
    pub fn new(admit: AdmitCache<A>, counter: C, auth_failure: Limit) -> Self {
        Self {
            admit,
            counter,
            auth_failure,
        }
    }

    /// Authenticate and admit, then rate limit. `presented` is what
    /// [`crate::auth::Authenticator::present`] found. `cred` is filled as far as it gets.
    pub async fn admit_and_limit(
        &self,
        matched: &Matched,
        ip: &str,
        presented: Result<Presented, AuthError>,
        cred: &mut RequestCredential,
    ) -> Result<Passed, Rejection> {
        let route = &matched.route;
        let admitted = match &route.scope {
            Scope::Public => Admitted::Public,
            _ => Admitted::Caller(self.admit(matched, ip, presented, cred).await?),
        };
        let rate_limit = self.limit(route, ip, &admitted).await?;
        Ok(Passed {
            admitted,
            rate_limit,
        })
    }

    async fn admit(
        &self,
        matched: &Matched,
        ip: &str,
        presented: Result<Presented, AuthError>,
        cred: &mut RequestCredential,
    ) -> Result<Caller, Rejection> {
        let route = &matched.route;
        let presented = match presented {
            Ok(p) => p,
            // Refused here, without identity: not counted.
            Err(err) => return Err(unauthorized(err, None, route.scope.name())),
        };
        let auth_type = presented.auth_type();
        cred.auth_type = Some(auth_type);
        let (credential, kid) = presented.into_admit();
        let req = admit_request(matched, credential);
        let answer = self
            .admit
            .get(&req)
            .await
            .map_err(|_| Rejection::Unavailable)?;
        cred.key_id = answer.key_id.clone();

        if !answer.valid {
            let rejection = unauthorized(
                AuthError::InvalidCredential,
                Some(auth_type),
                route.scope.name(),
            );
            return Err(self.count_refusal(ip).await.unwrap_or(rejection));
        }
        if service_account_on_user_route(&req, &answer) {
            // The credential is good: not counted by the failed-auth limiter.
            // Before the rate limit: a user route counts per user, and a service
            // account has none.
            cred.principal = answer.principal.clone();
            return Err(Rejection::ServiceAccount);
        }
        if answer.refused(&req) {
            // Before the rate limit, so a caller outside a tree can't spend its budget.
            return Err(self.count_refusal(ip).await.unwrap_or(Rejection::NotFound));
        }
        let caller = Caller {
            auth_type,
            key_id: answer.key_id.clone(),
            kid,
            principal: answer.principal.clone().unwrap_or_default(),
            nodes: answer.nodes.clone(),
            member_id: answer.member_id.clone().filter(|_| !req.chain.is_empty()),
            user_id: answer.user_id.clone().filter(|_| req.chain.is_empty()),
            permissions: answer.permissions.clone(),
        };
        cred.principal = Some(caller.principal.clone());
        cred.member_id = caller.member_id.clone();
        cred.user_id = caller.user_id.clone();
        cred.root_id = caller.root_id().map(str::to_string);
        cred.node_id = caller.node_id().map(str::to_string);
        Ok(caller)
    }

    /// The failed-auth IP limiter, on what identity refused. Some(rejection) when
    /// it answers instead.
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
            Err(Unavailable) => Some(Rejection::Unavailable),
        }
    }

    /// Every policy on the route, checked at once. Ok carries the tightest one's numbers.
    async fn limit(
        &self,
        route: &Route,
        ip: &str,
        admitted: &Admitted,
    ) -> Result<Option<RateLimitInfo>, Rejection> {
        let buckets = buckets(route, ip, admitted);
        match self.counter.check(&buckets).await {
            Ok(Verdict::Allowed(infos)) => Ok(tightest(infos)),
            Ok(Verdict::Exceeded { retry_after, infos }) => Err(Rejection::RateLimited {
                retry_after,
                info: tightest(infos),
            }),
            Err(Unavailable) => Err(Rejection::Unavailable),
        }
    }
}

/// A valid service account's answer on a user route (an empty chain): user
/// routes are a person's own, so the gateway refuses it with a **403**.
fn service_account_on_user_route(req: &AdmitRequest, answer: &AdmitAnswer) -> bool {
    answer.valid && req.chain.is_empty() && answer.principal.as_deref() == Some(SERVICE_ACCOUNT)
}

/// Admit's request for a matched route: the credential, the chain refs as
/// written in the path, and the route's `allow_deleted`.
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
                kid,
            } => (
                AdmitCredential::Idp {
                    issuer,
                    subject,
                    name,
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
            error_kind = ErrorKind::Network.as_str(),
            error_message = err.as_str(),
            "authentication infrastructure unavailable"
        );
        return Rejection::Unavailable;
    }
    let auth_type = auth_type.map_or("none", AuthType::as_str);
    metrics::record_auth_failure(auth_type, err.as_str());
    info!(
        error_kind = ErrorKind::Auth.as_str(),
        auth_type,
        error_message = err.as_str(),
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
                    (None, user) => format!("user:{}", user.as_deref().unwrap_or_default()),
                },
            };
            Bucket {
                key: format!("{}:{at}", policy.name),
                requests: policy.requests,
                window_seconds: policy.window_seconds,
            }
        })
        .collect()
}

/// The permission check, on a kind route.
pub fn authorize(route: &Route, admitted: &Admitted) -> Result<(), Rejection> {
    match admitted {
        Admitted::Caller(c) if !permitted(&route.required, &c.permissions) => {
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

/// The proxy's pipeline: identity over HTTP, Valkey counters.
pub type GatewayPipeline = Pipeline<crate::auth::admit::HttpAdmit, RateLimiter>;

#[cfg(test)]
mod tests;
