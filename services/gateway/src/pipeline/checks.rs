//! Route checks (docs/reference/gateway.md#route-checks): whether the caller may
//! call each of a batch of routes, decided by the pipeline's own steps up to the
//! permission check (match, admit from the cache, `required_permissions`), never
//! by a second rule. Rate limits, body limits, the fill and the upstream aren't
//! evaluated.

use serde_json::Value;

use super::{permitted, unauthorized, Counter, Pipeline, Rejection};
use crate::auth::admit::{credential_hash, Admit, AdmitCredential};
use crate::auth::{AuthError, Presented};
use crate::rate_limit::{Bucket, RateLimitInfo, Unavailable, Verdict};
use crate::route_map::{Policy, RouteMap, Scope};

/// The most checks one call takes.
pub const MAX_CHECKS: usize = 100;
/// The longest `path` a check takes, in bytes.
pub const MAX_CHECK_PATH: usize = 2048;
/// The largest body the call reads.
pub const MAX_CHECKS_BODY: u64 = 256 * 1024;
/// What the authentication log names as the scope of the call.
const CHECKS_SCOPE: &str = "route_checks";
/// The verbs a route takes.
const METHODS: &[&str] = &["GET", "POST", "PUT", "PATCH", "DELETE", "HEAD"];

/// One check: a request the caller might make.
#[derive(Clone, Debug, PartialEq, Eq)]
pub struct Check {
    pub method: String,
    pub path: String,
}

/// A field the body got wrong: its path (`checks[2].path`) and why.
#[derive(Clone, Debug, PartialEq, Eq)]
pub struct BadField {
    pub path: String,
    pub message: String,
}

fn bad(path: impl Into<String>, message: &str) -> BadField {
    BadField {
        path: path.into(),
        message: message.to_string(),
    }
}

/// The call's body: `{ "checks": [ { "method", "path" }, … ] }`, 1 to 100.
/// Other fields are ignored. Err names every field it got wrong.
pub fn parse_checks(body: &[u8]) -> Result<Vec<Check>, Vec<BadField>> {
    let value: Value = serde_json::from_slice(body)
        .map_err(|_| vec![bad("", "The body must be a JSON object.")])?;
    let Some(object) = value.as_object() else {
        return Err(vec![bad("", "The body must be a JSON object.")]);
    };
    let items = match object.get("checks").and_then(Value::as_array) {
        Some(items) if (1..=MAX_CHECKS).contains(&items.len()) => items,
        _ => {
            return Err(vec![bad(
                "checks",
                "checks must be a list of 1 to 100 checks.",
            )])
        }
    };
    let mut checks = Vec::with_capacity(items.len());
    let mut errs = Vec::new();
    for (i, item) in items.iter().enumerate() {
        let Some(item) = item.as_object() else {
            errs.push(bad(
                format!("checks[{i}]"),
                "A check is an object with a method and a path.",
            ));
            continue;
        };
        let method = item.get("method").and_then(Value::as_str);
        let method = match method {
            Some(m) if METHODS.contains(&m) => Some(m),
            _ => {
                errs.push(bad(
                    format!("checks[{i}].method"),
                    "method must be GET, POST, PUT, PATCH, DELETE or HEAD.",
                ));
                None
            }
        };
        let path = match item.get("path").and_then(Value::as_str) {
            Some(p) if public_path(p) => Some(p),
            _ => {
                errs.push(bad(
                    format!("checks[{i}].path"),
                    "path must start with /, be visible ASCII with no ? or #, and be at most 2048 characters.",
                ));
                None
            }
        };
        if let (Some(method), Some(path)) = (method, path) {
            checks.push(Check {
                method: method.to_string(),
                path: path.to_string(),
            });
        }
    }
    if errs.is_empty() {
        Ok(checks)
    } else {
        Err(errs)
    }
}

/// A public path as a request sends it: `/` first, visible ASCII, no query
/// string or fragment.
fn public_path(path: &str) -> bool {
    path.starts_with('/')
        && path.len() <= MAX_CHECK_PATH
        && path
            .bytes()
            .all(|b| b.is_ascii_graphic() && b != b'?' && b != b'#')
}

/// Why one check has no answer: the whole call is refused instead.
#[derive(Debug, PartialEq, Eq)]
pub enum CheckRefused {
    /// Admit refused the credential itself: **401**.
    Credential,
    /// Identity didn't answer: **503**.
    Unavailable,
}

/// The call's answer: each check's `allowed`, in order, and the `default`
/// policy's numbers for the response.
#[derive(Debug, PartialEq, Eq)]
pub struct Checked {
    pub allowed: Vec<bool>,
    pub rate_limit: Option<RateLimitInfo>,
}

impl<A: Admit, C: Counter> Pipeline<A, C> {
    /// The call's credential, as a route's: missing, malformed or a JWT refused
    /// locally is a **401**, JWKS not loaded a **503**.
    pub fn check_credential(
        presented: Result<Presented, AuthError>,
    ) -> Result<AdmitCredential, Rejection> {
        match presented {
            Ok(p) => Ok(p.into_admit().0),
            Err(err) => Err(unauthorized(err, None, CHECKS_SCOPE)),
        }
    }

    /// The whole call, once its credential and body are read: one count of
    /// `default` for the credential, then each check in order. A check admits
    /// as its request would; a check that needs no admit never calls identity.
    /// Admit refusing the credential on any check is the call's **401**,
    /// counted once by the failed-auth limiter.
    pub async fn check_routes(
        &self,
        routes: &RouteMap,
        default: &Policy,
        credential: &AdmitCredential,
        checks: &[Check],
        ip: &str,
    ) -> Result<Checked, Rejection> {
        let rate_limit = self.limit_credential(default, credential).await?;
        let mut allowed = Vec::with_capacity(checks.len());
        for check in checks {
            match self
                .check(routes, &check.method, &check.path, credential)
                .await
            {
                Ok(answer) => allowed.push(answer),
                Err(CheckRefused::Unavailable) => return Err(Rejection::Unavailable),
                Err(CheckRefused::Credential) => {
                    let auth_type = match credential {
                        AdmitCredential::Credential(_) => crate::auth::AuthType::ApiKey,
                        AdmitCredential::Idp { .. } => crate::auth::AuthType::Jwt,
                    };
                    let rejection =
                        unauthorized(AuthError::InvalidCredential, Some(auth_type), CHECKS_SCOPE);
                    return Err(self.count_refusal(ip).await.unwrap_or(rejection));
                }
            }
        }
        Ok(Checked {
            allowed,
            rate_limit,
        })
    }

    /// One check: what the pipeline would decide for this credential, method
    /// and path, up to the permission check.
    async fn check(
        &self,
        routes: &RouteMap,
        method: &str,
        path: &str,
        credential: &AdmitCredential,
    ) -> Result<bool, CheckRefused> {
        // Match. No route, or not this method: what a request would get as a 404.
        let Some(matched) = routes.find_route(path, method) else {
            return Ok(false);
        };
        let route = &matched.route;
        if route.scope == Scope::Public {
            return Ok(true);
        }
        // Admit, from the cache, with the route's chain (empty on a user route)
        // and allow_deleted.
        let req = super::admit_request(&matched, credential.clone());
        let answer = self
            .admit
            .get(&req)
            .await
            .map_err(|_| CheckRefused::Unavailable)?;
        if !answer.valid {
            return Err(CheckRefused::Credential);
        }
        // A service account on a user route: what a request would get as a 403.
        if super::service_account_on_user_route(&req, &answer) {
            return Ok(false);
        }
        // No such node, or not in its tree: what a request would get as a 403.
        if answer.refused(&req) {
            return Ok(false);
        }
        // The permission check. A user route requires nothing of a person.
        Ok(permitted(&route.required, &answer.permissions))
    }

    /// One count of `default` per credential: a route checks call, or a
    /// service account refused on a user route. The bucket is the same.
    pub(super) async fn limit_credential(
        &self,
        default: &Policy,
        credential: &AdmitCredential,
    ) -> Result<Option<RateLimitInfo>, Rejection> {
        let bucket = Bucket {
            key: format!(
                "{}:credential:{}",
                default.name,
                credential_hash(credential)
            ),
            requests: default.requests,
            window_seconds: default.window_seconds,
        };
        match self.counter.check(&[bucket]).await {
            Ok(Verdict::Allowed(infos)) => Ok(infos.into_iter().next()),
            Ok(Verdict::Exceeded { retry_after, infos }) => Err(Rejection::RateLimited {
                retry_after,
                info: infos.into_iter().next(),
            }),
            Err(Unavailable) => Err(Rejection::Unavailable),
        }
    }
}

#[cfg(test)]
mod tests {
    use super::*;

    fn paths(body: &str) -> Vec<String> {
        parse_checks(body.as_bytes())
            .unwrap_err()
            .into_iter()
            .map(|f| f.path)
            .collect()
    }

    fn many(n: usize) -> String {
        let one = r#"{ "method": "GET", "path": "/orgs/acme" }"#;
        format!(r#"{{ "checks": [{}] }}"#, vec![one; n].join(","))
    }

    #[test]
    fn a_batch_is_one_to_a_hundred_checks() {
        let checks = parse_checks(
            br#"{ "checks": [ { "method": "DELETE", "path": "/orgs/acme" } ], "x": 1 }"#,
        )
        .unwrap();
        assert_eq!(
            checks,
            vec![Check {
                method: "DELETE".into(),
                path: "/orgs/acme".into()
            }]
        );
        assert_eq!(
            parse_checks(many(MAX_CHECKS).as_bytes()).unwrap().len(),
            100
        );
        assert_eq!(paths(&many(MAX_CHECKS + 1)), vec!["checks"]);
        assert_eq!(paths(&many(0)), vec!["checks"]);
        assert_eq!(paths(r#"{}"#), vec!["checks"]);
        assert_eq!(paths(r#"{ "checks": {} }"#), vec!["checks"]);
    }

    #[test]
    fn a_malformed_body_names_each_field() {
        assert_eq!(paths("not json"), vec![""]);
        assert_eq!(paths(r#"[{ "method": "GET", "path": "/" }]"#), vec![""]);
        assert_eq!(paths(r#"{ "checks": ["GET /orgs"] }"#), vec!["checks[0]"]);
        for method in [
            r#""get""#,
            r#""OPTIONS""#,
            r#""TRACE""#,
            r#""""#,
            "1",
            "null",
        ] {
            let body = format!(r#"{{ "checks": [{{ "method": {method}, "path": "/a" }}] }}"#);
            assert_eq!(paths(&body), vec!["checks[0].method"], "{method}");
        }
        let long = format!(r#""/{}""#, "a".repeat(MAX_CHECK_PATH));
        for path in [
            r#""orgs/acme""#,
            r#""""#,
            r#""/orgs?x=1""#,
            r#""/orgs#top""#,
            r#""/orgs/a b""#,
            r#""/orgs/é""#,
            long.as_str(),
            "7",
        ] {
            let body = format!(r#"{{ "checks": [{{ "method": "GET", "path": {path} }}] }}"#);
            assert_eq!(paths(&body), vec!["checks[0].path"], "{path}");
        }
        // Every bad field, by index.
        assert_eq!(
            paths(r#"{ "checks": [ { "method": "GET", "path": "/ok" }, { "path": "x" }, 3 ] }"#),
            vec!["checks[1].method", "checks[1].path", "checks[2]"]
        );
        // The longest path passes.
        let longest = format!("/{}", "a".repeat(MAX_CHECK_PATH - 1));
        let body = format!(r#"{{ "checks": [{{ "method": "HEAD", "path": "{longest}" }}] }}"#);
        assert!(parse_checks(body.as_bytes()).is_ok());
    }
}
