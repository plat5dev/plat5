//! How the gateway decides a request (docs/concepts/access.md#how-a-request-is-decided),
//! apart from the proxy around it: authenticate and admit, rate limit, the label
//! check, and the subject fill. The proxy writes the audit intent between the rate
//! limit and the label check, and turns a [`Rejection`] into a response.

mod labels;
mod subject;

use std::collections::HashMap;

use std::future::Future;
use tracing::{info, warn};

use crate::auth::admit::{Admit, AdmitCache, AdmitCredential, AdmitRequest, ChainRef, NodeRef};
use crate::auth::shape::key_display_prefix;
use crate::auth::{AuthError, AuthType, Presented};
use crate::error::{ApiError, ErrorKind};
use crate::kinds::id_header;
use crate::metrics;
use crate::rate_limit::{RateLimitError, RateLimitInfo, RateLimiter};
use crate::route_map::{LimitBucket, Route, RouteLimiter, Scope};

pub use labels::satisfies_required_labels;
pub use subject::{path_and_query, RewriteError};

pub const MEMBER_ID_HEADER: &str = "X-Plat5-Member-Id";
pub const PRINCIPAL_HEADER: &str = "X-Plat5-Principal";

/// A fixed-window counter in a named bucket. Valkey in the gateway; in memory in tests.
pub trait Counter: Send + Sync {
    fn allow(
        &self,
        bucket: &str,
        limit: u64,
        window_seconds: u64,
    ) -> impl Future<Output = Result<RateLimitInfo, RateLimitError>> + Send;
}

impl Counter for RateLimiter {
    async fn allow(
        &self,
        bucket: &str,
        limit: u64,
        window_seconds: u64,
    ) -> Result<RateLimitInfo, RateLimitError> {
        RateLimiter::allow(self, bucket, limit, window_seconds).await
    }
}

/// A requests-per-window pair: the fallback, or the failed-auth IP limiter. `0` requests = off.
#[derive(Clone, Copy, Debug)]
pub struct Limit {
    pub requests: u64,
    pub window_seconds: u64,
}

/// The caller, once admitted on a kind or sign-in route.
#[derive(Clone, Debug, PartialEq, Eq)]
pub struct Caller {
    pub auth_type: AuthType,
    pub key_prefix: Option<String>,
    /// A JWT's key id, for the request span.
    pub kid: Option<String>,
    /// `person` or `service_account`.
    pub principal: String,
    /// The route's chain, root first. Empty on a sign-in route.
    pub nodes: Vec<NodeRef>,
    /// In this tree; on a sign-in route, the person's directory member.
    pub member_id: String,
    /// The caller's labels on the route's node. Log only: never sent on.
    pub labels: Vec<String>,
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
    /// Identity's display prefix for an `X-API-Key` of the right shape, valid or not.
    pub key_prefix: Option<String>,
    pub principal: Option<String>,
    pub root_id: Option<String>,
    pub node_id: Option<String>,
    pub member_id: Option<String>,
}

/// Why the gateway answers instead of forwarding.
#[derive(Debug, PartialEq)]
pub enum Rejection {
    /// **401**. `details` is a bad JWT's reason.
    Unauthorized(AuthError),
    /// **403** before the rate limit: no such node, or no member in its tree.
    NotFound,
    /// **403** after the rate limit: no route label held.
    LabelDenied,
    /// **429**. `info` is set on an admitted route, None on the failed-auth limiter.
    RateLimited {
        retry_after: u64,
        info: Option<RateLimitInfo>,
    },
    /// **400**: a path param that isn't one segment.
    BadPathParam,
    /// **500**: a subject id that isn't one segment, or a route the load check missed.
    Internal,
    /// **503**: identity, JWKS or Valkey.
    Unavailable,
}

impl Rejection {
    pub fn status(&self) -> u16 {
        match self {
            Rejection::Unauthorized(_) => 401,
            Rejection::NotFound | Rejection::LabelDenied => 403,
            Rejection::RateLimited { .. } => 429,
            Rejection::BadPathParam => 400,
            Rejection::Internal => 500,
            Rejection::Unavailable => 503,
        }
    }

    /// The body. Both 403s are the same, so neither says whether the node exists.
    pub fn body(&self, route: &Route) -> ApiError {
        match self {
            Rejection::Unauthorized(err) => ApiError::unauthorized_for(&route.scope, err.details()),
            Rejection::NotFound | Rejection::LabelDenied => {
                ApiError::forbidden(&route.required_labels)
            }
            Rejection::RateLimited { retry_after, .. } => ApiError::rate_limited(*retry_after),
            Rejection::BadPathParam => ApiError::invalid_request(),
            Rejection::Internal => ApiError::internal_error(),
            Rejection::Unavailable => ApiError::service_unavailable(),
        }
    }
}

/// The upstream request: the rewritten path (None = unchanged) and the subject headers.
#[derive(Debug, PartialEq, Eq)]
pub struct Forward {
    pub path: Option<String>,
    pub headers: Vec<(String, String)>,
}

pub struct Pipeline<A, C> {
    admit: AdmitCache<A>,
    counter: C,
    fallback: Limit,
    auth_failure: Limit,
}

impl<A: Admit, C: Counter> Pipeline<A, C> {
    pub fn new(admit: AdmitCache<A>, counter: C, fallback: Limit, auth_failure: Limit) -> Self {
        Self {
            admit,
            counter,
            fallback,
            auth_failure,
        }
    }

    /// Steps 2 and 3: authenticate and admit, then rate limit. `presented` is what
    /// [`crate::auth::Authenticator::present`] found. `cred` is filled as far as it gets.
    pub async fn admit_and_limit(
        &self,
        route: &Route,
        params: &HashMap<String, String>,
        method: &str,
        ip: &str,
        presented: Result<Presented, AuthError>,
        cred: &mut RequestCredential,
    ) -> Result<Passed, Rejection> {
        let admitted = match &route.scope {
            Scope::Public => Admitted::Public,
            _ => Admitted::Caller(self.admit(route, params, ip, presented, cred).await?),
        };
        let rate_limit = self.limit(route, method, ip, &admitted).await?;
        Ok(Passed {
            admitted,
            rate_limit,
        })
    }

    async fn admit(
        &self,
        route: &Route,
        params: &HashMap<String, String>,
        ip: &str,
        presented: Result<Presented, AuthError>,
        cred: &mut RequestCredential,
    ) -> Result<Caller, Rejection> {
        let presented = match presented {
            Ok(p) => p,
            Err(err) => return Err(self.unauthorized(err, None, &route.scope, ip).await),
        };
        let auth_type = presented.auth_type();
        cred.auth_type = Some(auth_type);
        let (secret, credential, kid) = match presented {
            Presented::Key(key) => {
                cred.key_prefix = key_display_prefix(&key);
                if cred.key_prefix.is_none() {
                    // The wrong shape never reaches identity.
                    return Err(self
                        .unauthorized(AuthError::InvalidKey, Some(auth_type), &route.scope, ip)
                        .await);
                }
                (key.clone(), AdmitCredential::Credential(key), None)
            }
            Presented::Jwt {
                token,
                issuer,
                subject,
                name,
                kid,
            } => (
                token,
                AdmitCredential::Idp {
                    issuer,
                    subject,
                    name,
                },
                kid,
            ),
        };

        let mut chain = Vec::with_capacity(route.chain.len());
        for kind in &route.chain {
            let Some(reference) = params.get(kind) else {
                warn!(route = %route.path, kind = %kind, "chain param missing from a matched path");
                return Err(Rejection::Internal);
            };
            chain.push(ChainRef {
                kind: kind.clone(),
                reference: reference.clone(),
            });
        }
        let req = AdmitRequest {
            credential,
            scope: route.scope.name().to_string(),
            chain,
            resolve_deleted: route.resolve_deleted,
        };
        let answer = self
            .admit
            .get(&secret, &req)
            .await
            .map_err(|_| Rejection::Unavailable)?;

        if !answer.valid {
            let err = match auth_type {
                AuthType::ApiKey => AuthError::InvalidKey,
                AuthType::Jwt => AuthError::InvalidToken {
                    reason: "inactive_person",
                },
            };
            return Err(self
                .unauthorized(err, Some(auth_type), &route.scope, ip)
                .await);
        }
        let member_id = match (&answer.member_id, answer.found) {
            (Some(member_id), true) => member_id.clone(),
            _ => {
                // Before the rate limit, so a caller outside a tree can't spend its budget.
                return Err(self
                    .note_unadmitted(ip)
                    .await
                    .unwrap_or(Rejection::NotFound));
            }
        };
        let caller = Caller {
            auth_type,
            key_prefix: cred.key_prefix.clone(),
            kid,
            principal: answer.principal.clone().unwrap_or_default(),
            nodes: answer.nodes.clone(),
            member_id,
            labels: answer.labels.clone(),
        };
        cred.principal = Some(caller.principal.clone());
        cred.member_id = Some(caller.member_id.clone());
        cred.root_id = caller.root_id().map(str::to_string);
        cred.node_id = caller.node_id().map(str::to_string);
        Ok(caller)
    }

    async fn unauthorized(
        &self,
        err: AuthError,
        auth_type: Option<AuthType>,
        scope: &Scope,
        ip: &str,
    ) -> Rejection {
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
            scope = scope.name(),
            "authentication failed"
        );
        self.note_unadmitted(ip)
            .await
            .unwrap_or(Rejection::Unauthorized(err))
    }

    /// The failed-auth IP limiter: unadmitted 401s, not-found 403s and unmatched
    /// 404s. Some(rejection) when it answers instead.
    pub async fn note_unadmitted(&self, ip: &str) -> Option<Rejection> {
        match self
            .counter
            .allow(
                &format!("ip:{ip}"),
                self.auth_failure.requests,
                self.auth_failure.window_seconds,
            )
            .await
        {
            Ok(_) => None,
            Err(RateLimitError::Exceeded { retry_after, .. }) => Some(Rejection::RateLimited {
                retry_after,
                info: None,
            }),
            Err(RateLimitError::Unavailable) => Some(Rejection::Unavailable),
        }
    }

    /// Step 3: every policy on the route counts in its node's bucket, and every one
    /// must pass. Ok carries the tightest policy's numbers.
    async fn limit(
        &self,
        route: &Route,
        method: &str,
        ip: &str,
        admitted: &Admitted,
    ) -> Result<Option<RateLimitInfo>, Rejection> {
        let buckets = buckets(route, method, ip, admitted, self.fallback)?;
        let mut passed: Vec<RateLimitInfo> = Vec::new();
        let mut exceeded: Vec<(u64, RateLimitInfo)> = Vec::new();
        for (bucket, requests, window) in &buckets {
            match self.counter.allow(bucket, *requests, *window).await {
                Ok(info) => passed.push(info),
                Err(RateLimitError::Exceeded { retry_after, info }) => {
                    exceeded.push((retry_after, info))
                }
                Err(RateLimitError::Unavailable) => return Err(Rejection::Unavailable),
            }
        }
        if !exceeded.is_empty() {
            let retry_after = exceeded.iter().map(|(r, _)| *r).max().unwrap_or(1);
            let info = tightest(exceeded.into_iter().map(|(_, i)| i));
            return Err(Rejection::RateLimited { retry_after, info });
        }
        Ok(tightest(passed.into_iter()))
    }
}

/// The policy with the fewest requests left; on a tie, the one that resets last.
fn tightest(infos: impl Iterator<Item = RateLimitInfo>) -> Option<RateLimitInfo> {
    infos.min_by(|a, b| {
        a.remaining
            .cmp(&b.remaining)
            .then_with(|| b.reset_epoch.cmp(&a.reset_epoch))
    })
}

/// Each policy's bucket, with its requests and window
/// (docs/reference/config/routes.md#buckets).
fn buckets(
    route: &Route,
    method: &str,
    ip: &str,
    admitted: &Admitted,
    fallback: Limit,
) -> Result<Vec<(String, u64, u64)>, Rejection> {
    let node = |count_at: Option<&str>| -> Result<String, Rejection> {
        match (&route.scope, admitted) {
            (Scope::Public, _) => Ok(ip.to_string()),
            (Scope::SignIn(_), Admitted::Caller(c)) => Ok(c.member_id.clone()),
            (Scope::Kind(kind), Admitted::Caller(c)) => {
                let at = count_at.unwrap_or(kind);
                c.nodes
                    .iter()
                    .find(|n| n.kind == at)
                    .map(|n| n.id.clone())
                    .ok_or(Rejection::Internal)
            }
            _ => Err(Rejection::Internal),
        }
    };
    let method_path = |node: String| format!("{method} {} {node}", route.path);
    match &route.limiter {
        RouteLimiter::Unlimited => Ok(Vec::new()),
        RouteLimiter::Inherit if fallback.requests == 0 => Ok(Vec::new()),
        RouteLimiter::Inherit => Ok(vec![(
            method_path(node(None)?),
            fallback.requests,
            fallback.window_seconds,
        )]),
        RouteLimiter::Policies(policies) => policies
            .iter()
            .map(|p| {
                let at = node(p.count_at.as_deref())?;
                let bucket = match &p.bucket {
                    LimitBucket::MethodPath => method_path(at),
                    LimitBucket::Named(prefix) => format!("{prefix}:{at}"),
                };
                Ok((bucket, p.requests, p.window_seconds))
            })
            .collect(),
    }
}

/// Step 5: the caller's labels on the node against the route's.
pub fn authorize(route: &Route, admitted: &Admitted) -> Result<(), Rejection> {
    match admitted {
        Admitted::Public => Ok(()),
        Admitted::Caller(c) if satisfies_required_labels(&route.required_labels, &c.labels) => {
            Ok(())
        }
        Admitted::Caller(_) => Err(Rejection::LabelDenied),
    }
}

/// Step 6: the upstream path and the subject headers. Nothing on a public route.
pub fn forward(
    route: &Route,
    params: &HashMap<String, String>,
    admitted: &Admitted,
) -> Result<Forward, Rejection> {
    let (nodes, member_id) = match admitted {
        Admitted::Public => (&[][..], None),
        Admitted::Caller(c) => (c.nodes.as_slice(), Some(c.member_id.as_str())),
    };
    let path = match route.upstream.as_deref() {
        None => None,
        Some(template) => Some(
            subject::substitute(template, params, subject::Subject { nodes, member_id }).map_err(
                |e| match e {
                    RewriteError::PathParam => Rejection::BadPathParam,
                    RewriteError::Subject | RewriteError::Internal => Rejection::Internal,
                },
            )?,
        ),
    };
    let mut headers = Vec::new();
    if let Admitted::Caller(c) = admitted {
        for node in &c.nodes {
            headers.push((id_header(&node.kind), node.id.clone()));
        }
        headers.push((MEMBER_ID_HEADER.to_string(), c.member_id.clone()));
        headers.push((PRINCIPAL_HEADER.to_string(), c.principal.clone()));
    }
    Ok(Forward { path, headers })
}

/// The proxy's pipeline: identity over HTTP, Valkey counters.
pub type GatewayPipeline = Pipeline<crate::auth::admit::HttpAdmit, RateLimiter>;

#[cfg(test)]
mod tests;
