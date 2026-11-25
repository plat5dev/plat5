//! Outbound OpenID AuthZEN 1.0 policy decision point (PDP) client and TTL
//! decision cache (docs/reference/config/authorizers.md).
//!
//! When a route declares an `authorizer`, the gateway delegates (or combines)
//! the authorization check with an external AuthZEN 1.0 PDP via
//! `POST {url}/access/v1/evaluation` (single check) or
//! `POST {url}/access/v1/evaluations` (multi-permission OR routes and batched
//! `POST /access/routes` UI checks).

mod cache;
mod client;
mod config;

pub use cache::AuthorizerCache;
pub use client::{
    Authorizer, AuthorizerUnavailable, AuthzenAction, AuthzenRequest, AuthzenResource,
    AuthzenSubject, BatchSemantic, HttpAuthorizer, NoAuthorizer, MAX_BATCH_EVALUATIONS,
};
pub use config::{
    parse_authorizers, parse_authorizers_with_env, AuthorizerDoc, AuthorizerMode, AuthorizerSpec,
    RouteAuthorizer, RouteAuthorizerDoc, RouteResource, RouteResourceDoc,
};
