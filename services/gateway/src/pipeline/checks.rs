//! Route checks (docs/reference/gateway.md#route-checks): whether the caller may
//! call each of a batch of routes, decided by the pipeline's own `admit_credential`
//! and `prepare_authorization` steps, never by a second rule. Rate limits, body
//! limits, the fill and the upstream aren't evaluated.

use std::collections::BTreeMap;

use serde_json::Value;

use super::{
    prepare_authorization, unauthorized, AdmitOutcome, AuthzDecision, Counter, Pipeline, Rejection,
};
use crate::auth::admit::{credential_hash, Admit, AdmitCredential};
use crate::auth::{AuthError, Presented};
use crate::authorizer::{Authorizer, AuthorizerSpec, AuthzenRequest};
use crate::rate_limit::{Bucket, RateLimitInfo};
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
enum CheckRefused {
    /// Admit refused the credential itself: **401**.
    Credential,
    /// Identity didn't answer: **503**.
    Unavailable,
}

enum CheckOutcome {
    Decided(bool),
    External(String, Vec<AuthzenRequest>),
}

/// The call's answer: each check's `allowed`, in order, and the tightest
/// default policy's numbers for the response.
#[derive(Debug, PartialEq, Eq)]
pub struct Checked {
    pub allowed: Vec<bool>,
    pub rate_limit: Option<RateLimitInfo>,
}

impl<A: Admit, C: Counter, P: Authorizer> Pipeline<A, C, P> {
    /// The call's credential, as a route's: missing, malformed or a JWT refused
    /// locally is a **401**, JWKS not loaded a **503**.
    pub fn check_credential(
        presented: Result<Presented, AuthError>,
    ) -> Result<AdmitCredential, Rejection> {
        match presented {
            Ok(p) => Ok(p.into_admit().0),
            Err(err) => {
                let auth_type = err.auth_type();
                Err(unauthorized(err, auth_type, CHECKS_SCOPE))
            }
        }
    }

    /// The whole call, once its credential and body are read: one count of
    /// each policy `defaults.rate_limit` names for the credential, then each
    /// check in order.
    pub async fn check_routes(
        &self,
        routes: &RouteMap,
        defaults: &[Policy],
        credential: &AdmitCredential,
        checks: &[Check],
        ip: &str,
        host: Option<&str>,
    ) -> Result<Checked, Rejection> {
        self.check_routes_with_authorizers(
            routes,
            defaults,
            &BTreeMap::new(),
            credential,
            checks,
            ip,
            host,
            "",
        )
        .await
    }

    #[allow(clippy::too_many_arguments)]
    pub async fn check_routes_with_authorizers(
        &self,
        routes: &RouteMap,
        defaults: &[Policy],
        authorizers: &BTreeMap<String, AuthorizerSpec>,
        credential: &AdmitCredential,
        checks: &[Check],
        ip: &str,
        host: Option<&str>,
        request_id: &str,
    ) -> Result<Checked, Rejection> {
        let rate_limit = self.limit_credential(defaults, credential).await?;
        let mut allowed = vec![false; checks.len()];
        let mut pending: BTreeMap<String, Vec<(usize, AuthzenRequest)>> = BTreeMap::new();

        for (idx, check) in checks.iter().enumerate() {
            match self
                .check_one(routes, check, credential, ip, host, request_id)
                .await
            {
                Ok(CheckOutcome::Decided(ans)) => allowed[idx] = ans,
                Ok(CheckOutcome::External(name, reqs)) => {
                    let group = pending.entry(name).or_default();
                    for req in reqs {
                        group.push((idx, req));
                    }
                }
                Err(CheckRefused::Unavailable) => return Err(Rejection::Unavailable),
                Err(CheckRefused::Credential) => {
                    let rejection = unauthorized(
                        AuthError::InvalidCredential,
                        credential.auth_type(),
                        CHECKS_SCOPE,
                    );
                    return Err(self.count_refusal(ip).await.unwrap_or(rejection));
                }
            }
        }

        for (name, items) in pending {
            let spec = authorizers.get(&name).ok_or(Rejection::Unavailable)?;
            let reqs: Vec<AuthzenRequest> = items.iter().map(|(_, r)| r.clone()).collect();
            let decisions = self
                .authorizer
                .evaluate_all(spec, &reqs, request_id)
                .await
                .map_err(|_| Rejection::Unavailable)?;
            for ((idx, _), decision) in items.into_iter().zip(decisions) {
                if decision {
                    allowed[idx] = true;
                }
            }
        }

        Ok(Checked {
            allowed,
            rate_limit,
        })
    }

    /// One check: delegates to the pipeline's own `admit_credential` and
    /// `prepare_authorization`, returning `External` so checks batch per PDP.
    async fn check_one(
        &self,
        routes: &RouteMap,
        check: &Check,
        credential: &AdmitCredential,
        ip: &str,
        host: Option<&str>,
        request_id: &str,
    ) -> Result<CheckOutcome, CheckRefused> {
        let Some(matched) = routes.find_route_with_host(&check.path, &check.method, host) else {
            return Ok(CheckOutcome::Decided(false));
        };
        if matched.route.scope == Scope::Public {
            return Ok(CheckOutcome::Decided(true));
        }
        let (_, outcome) = self
            .admit_credential(&matched, credential.clone(), None, ip)
            .await;
        let caller = match outcome {
            AdmitOutcome::Admitted(caller) => caller,
            AdmitOutcome::InvalidCredential { .. } => return Err(CheckRefused::Credential),
            AdmitOutcome::Unavailable => return Err(CheckRefused::Unavailable),
            AdmitOutcome::ServiceAccount { .. }
            | AdmitOutcome::ScopedCredential { .. }
            | AdmitOutcome::NotAdmitted { .. } => return Ok(CheckOutcome::Decided(false)),
        };
        match prepare_authorization(&matched, &caller, ip, request_id) {
            Ok(AuthzDecision::Allow) => Ok(CheckOutcome::Decided(true)),
            Ok(AuthzDecision::Deny) | Err(_) => Ok(CheckOutcome::Decided(false)),
            Ok(AuthzDecision::External {
                authorizer,
                requests,
            }) => Ok(CheckOutcome::External(authorizer, requests)),
        }
    }

    /// One count of each policy `defaults.rate_limit` names, per credential,
    /// for a `POST /access/routes` call.
    pub(super) async fn limit_credential(
        &self,
        defaults: &[Policy],
        credential: &AdmitCredential,
    ) -> Result<Option<RateLimitInfo>, Rejection> {
        let hash = credential_hash(credential);
        let buckets: Vec<Bucket> = defaults
            .iter()
            .map(|p| Bucket {
                key: format!("{}:credential:{hash}", p.name),
                requests: p.requests,
                window_seconds: p.window_seconds,
            })
            .collect();
        self.check_buckets(&buckets, "route_checks").await
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
        assert_eq!(
            paths(r#"{ "checks": [ { "method": "GET", "path": "/ok" }, { "path": "x" }, 3 ] }"#),
            vec!["checks[1].method", "checks[1].path", "checks[2]"]
        );
        let longest = format!("/{}", "a".repeat(MAX_CHECK_PATH - 1));
        let body = format!(r#"{{ "checks": [{{ "method": "HEAD", "path": "{longest}" }}] }}"#);
        assert!(parse_checks(body.as_bytes()).is_ok());
    }
}
