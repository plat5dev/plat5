//! Outbound OpenID AuthZEN 1.0 policy decision point (PDP) client and TTL
//! decision cache (docs/reference/config/authorizer.md).
//!
//! When the deployment configures a top-level `authorizer`, the gateway
//! delegates every permission check to that external AuthZEN 1.0 PDP via
//! `POST {url}/access/v1/evaluation` (single check) or
//! `POST {url}/access/v1/evaluations` (multi-permission OR routes and batched
//! `POST /access/routes` UI checks).

mod cache;
mod client;
mod config;

pub use cache::AuthorizerCache;
pub use client::{
    Authorizer, AuthorizerUnavailable, AuthzenAction, AuthzenRequest, AuthzenResource,
    AuthzenSubject, BatchSemantic, HttpAuthorizer, NoAuthorizer, AUTHORIZER_METRIC_LABEL,
    MAX_BATCH_EVALUATIONS,
};
pub use config::{
    parse_authorizer, parse_authorizer_with_env, AuthorizerDoc, AuthorizerSpec, RouteResource,
    RouteResourceDoc,
};
