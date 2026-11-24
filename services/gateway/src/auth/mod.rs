//! Who presented a request: the `Authorization: Bearer` value, as a verified JWT's
//! issuer and subject or as a key (docs/concepts/access.md#credentials). Admit then
//! says whose it is.

pub mod admit;
pub mod call;
pub mod invalidations;
pub mod jwt;

use std::collections::BTreeMap;

use pingora::http::RequestHeader;
use serde_json::Value;

use self::jwt::{is_unexpired, validate_token, JwtCache, JwtValidatorState};

const JWT_CACHE_CAPACITY: u64 = 10_000;
const JWT_CACHE_TTL_BUFFER_SECS: u64 = 60;
const MAX_NAME_CHARS: usize = 128;

/// Credential kind, as logs and audit events name it.
#[derive(Debug, Clone, Copy, PartialEq, Eq)]
pub enum AuthType {
    Jwt,
    ApiKey,
}

impl AuthType {
    pub fn as_str(self) -> &'static str {
        match self {
            AuthType::Jwt => "jwt",
            AuthType::ApiKey => "apikey",
        }
    }
}

/// The credential on a request, before admit.
#[derive(Clone, Debug, PartialEq, Eq)]
pub enum Presented {
    /// A bearer value that isn't a JWT, as sent: identity owns the key format.
    Key(String),
    /// A verified JWT.
    Jwt {
        issuer: String,
        subject: String,
        name: Option<String>,
        scopes: Option<Vec<String>>,
        policy_claims: BTreeMap<String, Vec<String>>,
        kid: Option<String>,
    },
}

impl Presented {
    pub fn auth_type(&self) -> AuthType {
        match self {
            Presented::Key(_) => AuthType::ApiKey,
            Presented::Jwt { .. } => AuthType::Jwt,
        }
    }
}

#[derive(Debug, Clone, PartialEq, Eq)]
pub enum AuthError {
    MissingAuthorization,
    InvalidAuthorizationHeader,
    InvalidToken {
        reason: &'static str,
    },
    MissingSubject,
    /// Identity answered `valid: false`.
    InvalidCredential,
    /// JWKS isn't loaded: **503**.
    JwtValidationUnavailable,
}

impl AuthError {
    pub fn as_str(&self) -> &'static str {
        match self {
            AuthError::MissingAuthorization => "missing_authorization",
            AuthError::InvalidAuthorizationHeader => "invalid_authorization",
            AuthError::InvalidToken { reason } => reason,
            AuthError::MissingSubject => "missing_subject",
            AuthError::InvalidCredential => "invalid_credential",
            AuthError::JwtValidationUnavailable => "jwks_unavailable",
        }
    }

    /// A **401**; anything else is the gateway's own trouble.
    pub fn is_client_error(&self) -> bool {
        !matches!(self, AuthError::JwtValidationUnavailable)
    }

    /// `details` on the 401: the reason for a bad JWT only.
    pub fn details(&self) -> Option<Value> {
        match self {
            AuthError::InvalidToken { reason } => Some(serde_json::json!({ "reason": *reason })),
            _ => None,
        }
    }

    /// Credential kind when known locally before admit (locally refused JWTs).
    pub fn auth_type(&self) -> Option<AuthType> {
        match self {
            AuthError::InvalidToken { .. } | AuthError::MissingSubject => Some(AuthType::Jwt),
            _ => None,
        }
    }
}

/// `AUTH_NAME_CLAIMS`, each as path segments, in order.
pub type NameClaims = Vec<Vec<String>>;

/// Three dot-separated segments is a JWT; anything else is a key.
pub fn is_jwt(token: &str) -> bool {
    token.split('.').count() == 3
}

/// JWT verification and its claim cache.
pub struct Authenticator {
    jwt_validator: JwtValidatorState,
    jwt_cache: JwtCache,
    subject_claim: Vec<String>,
    scope_claim: Option<Vec<String>>,
    policy_claims: Vec<(String, Vec<String>)>,
}

impl Authenticator {
    pub fn new(
        jwt_validator: JwtValidatorState,
        subject_claim: Vec<String>,
        scope_claim: Option<Vec<String>>,
        policy_claims: Vec<(String, Vec<String>)>,
    ) -> Self {
        Self {
            jwt_validator,
            jwt_cache: JwtCache::new(JWT_CACHE_CAPACITY, JWT_CACHE_TTL_BUFFER_SECS),
            subject_claim,
            scope_claim,
            policy_claims,
        }
    }

    /// The request's credential, from `Authorization: Bearer`. `name_claims` is
    /// `AUTH_NAME_CLAIMS`: a JWT's name is the first of them that's non-empty.
    pub async fn present(
        &self,
        req: &RequestHeader,
        name_claims: &[Vec<String>],
    ) -> Result<Presented, AuthError> {
        let authorization = req
            .headers
            .get("Authorization")
            .ok_or(AuthError::MissingAuthorization)?
            .to_str()
            .map_err(|_| AuthError::InvalidAuthorizationHeader)?;
        let mut parts = authorization.split_whitespace();
        let token = match (parts.next(), parts.next(), parts.next()) {
            (Some("Bearer"), Some(token), None) => token,
            _ => return Err(AuthError::InvalidAuthorizationHeader),
        };
        if !is_jwt(token) {
            return Ok(Presented::Key(token.to_string()));
        }

        let (claims, kid) = match self.jwt_cache.get(token).await {
            Some(cached) => {
                if !is_unexpired(&cached.claims) {
                    self.jwt_cache.invalidate(token).await;
                    return Err(AuthError::InvalidToken {
                        reason: "token_expired",
                    });
                }
                let kid = cached.header.kid.clone();
                (cached.claims, kid)
            }
            None => {
                let jwks = self
                    .jwt_validator
                    .get_jwks()
                    .await
                    .map_err(|_| AuthError::JwtValidationUnavailable)?;
                let (data, kid) = validate_token(
                    token,
                    self.jwt_validator.get_issuer(),
                    jwks,
                    self.jwt_validator.get_allowed_audiences().clone(),
                )
                .await
                .map_err(|e| AuthError::InvalidToken {
                    reason: jwt_error_reason(&e),
                })?;
                self.jwt_cache.put(token, data.clone()).await;
                (data.claims, Some(kid))
            }
        };

        let subject =
            extract_claim_path(&claims, &self.subject_claim).ok_or(AuthError::MissingSubject)?;
        let scopes = self
            .scope_claim
            .as_deref()
            .and_then(|path| extract_scopes(&claims, path));
        let policy_claims = extract_policy_claims(&claims, &self.policy_claims);
        Ok(Presented::Jwt {
            // Validation checked `iss` against the configured issuer.
            issuer: self.jwt_validator.get_issuer(),
            subject,
            name: first_name(&claims, name_claims),
            scopes,
            policy_claims,
            kid,
        })
    }
}

pub fn jwt_error_reason(err: &jsonwebtoken::errors::Error) -> &'static str {
    use jsonwebtoken::errors::ErrorKind;
    match err.kind() {
        ErrorKind::ExpiredSignature => "token_expired",
        ErrorKind::InvalidSignature => "invalid_signature",
        ErrorKind::InvalidToken => "malformed_token",
        ErrorKind::InvalidIssuer => "invalid_issuer",
        ErrorKind::InvalidAudience => "invalid_audience",
        ErrorKind::InvalidSubject => "invalid_subject",
        _ => "invalid_token",
    }
}

/// Walk a dotted path into JWT claims JSON (e.g. `sub`, `properties.user_id`).
/// A missing or empty string is None.
pub fn extract_claim_path(claims: &Value, path: &[String]) -> Option<String> {
    let mut cur = claims;
    for key in path {
        cur = cur.get(key)?;
    }
    let s = cur.as_str()?;
    if s.is_empty() {
        return None;
    }
    Some(s.to_string())
}

/// Walk a dotted path into JWT claims JSON for OAuth scopes (`AUTH_SCOPE_CLAIM`).
/// Accepts a space-delimited string or a JSON array of strings, returning sorted,
/// deduplicated values. Missing, `null`, or non-string/array values return `None`.
pub fn extract_scopes(claims: &Value, path: &[String]) -> Option<Vec<String>> {
    if path.is_empty() {
        return None;
    }
    let mut cur = claims;
    for key in path {
        cur = cur.get(key)?;
    }
    let mut scopes: Vec<String> = match cur {
        Value::String(s) => s.split_whitespace().map(str::to_string).collect(),
        Value::Array(items) => items
            .iter()
            .filter_map(Value::as_str)
            .map(str::trim)
            .filter(|s| !s.is_empty())
            .map(str::to_string)
            .collect(),
        _ => return None,
    };
    scopes.sort();
    scopes.dedup();
    Some(scopes)
}

/// Extract discrete claim values configured by `AUTH_POLICY_CLAIMS` from JWT
/// claims JSON. Accepts string, bool, number, or an array of those values,
/// returning sorted, deduplicated strings per claim name.
pub fn extract_policy_claims(
    claims: &Value,
    paths: &[(String, Vec<String>)],
) -> BTreeMap<String, Vec<String>> {
    let mut out = BTreeMap::new();
    for (canonical, path) in paths {
        if path.is_empty() {
            continue;
        }
        let mut cur = claims;
        let mut found = true;
        for key in path {
            let Some(next) = cur.get(key) else {
                found = false;
                break;
            };
            cur = next;
        }
        if !found {
            continue;
        }
        let mut vals: Vec<String> = match cur {
            Value::String(s) => {
                let t = s.trim();
                if t.is_empty() {
                    Vec::new()
                } else {
                    vec![t.to_string()]
                }
            }
            Value::Bool(b) => vec![b.to_string()],
            Value::Number(n) => vec![n.to_string()],
            Value::Array(items) => items
                .iter()
                .filter_map(|item| match item {
                    Value::String(s) => {
                        let t = s.trim();
                        (!t.is_empty()).then(|| t.to_string())
                    }
                    Value::Bool(b) => Some(b.to_string()),
                    Value::Number(n) => Some(n.to_string()),
                    _ => None,
                })
                .collect(),
            _ => Vec::new(),
        };
        vals.sort();
        vals.dedup();
        if !vals.is_empty() {
            out.insert(canonical.clone(), vals);
        }
    }
    out
}

/// The first of `paths` that holds a string with something besides spaces,
/// trimmed and truncated to 128 characters: the person's name
/// (docs/reference/idp.md#a-persons-name).
pub fn first_name(claims: &Value, paths: &[Vec<String>]) -> Option<String> {
    paths.iter().find_map(|path| {
        let name = extract_claim_path(claims, path)?;
        let truncated: String = name.trim().chars().take(MAX_NAME_CHARS).collect();
        let trimmed = truncated.trim();
        (!trimmed.is_empty()).then(|| trimmed.to_string())
    })
}

/// A dotted claim path (`AUTH_SUBJECT_CLAIM`, each of `AUTH_NAME_CLAIMS`) as
/// path segments, each trimmed, empty ones dropped.
pub fn parse_claim_path(raw: &str) -> Vec<String> {
    raw.split('.')
        .map(|s| s.trim().to_string())
        .filter(|s| !s.is_empty())
        .collect()
}

/// Parse `AUTH_SUBJECT_CLAIM` into path segments. Empty → the default, `sub`.
pub fn parse_subject_claim(raw: &str) -> Vec<String> {
    let parts = parse_claim_path(raw);
    if parts.is_empty() {
        vec!["sub".to_string()]
    } else {
        parts
    }
}

#[cfg(test)]
mod tests {
    use super::*;
    use crate::auth::jwt::Audiences;
    use serde_json::json;

    #[test]
    fn parse_default_and_nested() {
        assert_eq!(
            parse_subject_claim("properties.user_id"),
            vec!["properties", "user_id"]
        );
        assert_eq!(parse_subject_claim("sub"), vec!["sub"]);
        assert_eq!(parse_subject_claim(""), vec!["sub"]);
    }

    #[test]
    fn extract_nested_and_sub() {
        let claims = json!({"properties": {"user_id": "01ABC"}, "sub": "auth0|1", "name": ""});
        assert_eq!(
            extract_claim_path(&claims, &parse_subject_claim("properties.user_id")).as_deref(),
            Some("01ABC")
        );
        assert_eq!(
            extract_claim_path(&claims, &parse_subject_claim("sub")).as_deref(),
            Some("auth0|1")
        );
        assert!(extract_claim_path(&claims, &parse_subject_claim("missing")).is_none());
        assert!(extract_claim_path(&claims, &["name".to_string()]).is_none());
    }

    fn paths(raw: &[&str]) -> NameClaims {
        raw.iter().map(|c| parse_claim_path(c)).collect()
    }

    #[test]
    fn the_name_is_the_first_non_empty_claim_in_order() {
        let claims = json!({
            "name": "  ", "nickname": "", "preferred_username": "  dana ",
            "profile": { "display_name": "Dana Q", "n": 3 }, "email": "dana@example.com"
        });
        // Blank, empty, missing and non-string claims are skipped.
        assert_eq!(
            first_name(
                &claims,
                &paths(&[
                    "name",
                    "nickname",
                    "missing",
                    "profile.n",
                    "preferred_username"
                ])
            )
            .as_deref(),
            Some("dana")
        );
        assert_eq!(
            first_name(
                &claims,
                &paths(&["profile.display_name", "preferred_username"])
            )
            .as_deref(),
            Some("Dana Q")
        );
        assert_eq!(
            first_name(
                &claims,
                &paths(&["preferred_username", "profile.display_name"])
            )
            .as_deref(),
            Some("dana")
        );
        assert_eq!(first_name(&claims, &paths(&["name", "nickname"])), None);
        assert_eq!(first_name(&claims, &[]), None);
        assert_eq!(
            parse_claim_path("profile.display_name"),
            vec!["profile", "display_name"]
        );
    }

    fn authenticator() -> Authenticator {
        Authenticator::new(
            JwtValidatorState::new(
                "https://auth".into(),
                "http://127.0.0.1:1/jwks".into(),
                Audiences::Any,
            ),
            parse_subject_claim("sub"),
            None,
            Vec::new(),
        )
    }

    #[test]
    fn extract_policy_claims_handles_scalars_arrays_and_nested() {
        let claims = json!({
            "amr": ["mfa", "pwd", "mfa", " "],
            "acr": "urn:okta:loa:2fa",
            "email_verified": true,
            "org": { "idp_connection": "okta-acme" },
            "empty": "",
            "obj": { "a": 1 }
        });
        let paths = vec![
            ("amr".into(), parse_claim_path("amr")),
            ("acr".into(), parse_claim_path("acr")),
            ("email_verified".into(), parse_claim_path("email_verified")),
            (
                "org.idp_connection".into(),
                parse_claim_path("org.idp_connection"),
            ),
            ("empty".into(), parse_claim_path("empty")),
            ("missing".into(), parse_claim_path("missing")),
            ("obj".into(), parse_claim_path("obj")),
        ];
        let got = extract_policy_claims(&claims, &paths);
        assert_eq!(got.get("amr"), Some(&vec!["mfa".into(), "pwd".into()]));
        assert_eq!(got.get("acr"), Some(&vec!["urn:okta:loa:2fa".into()]));
        assert_eq!(got.get("email_verified"), Some(&vec!["true".into()]));
        assert_eq!(
            got.get("org.idp_connection"),
            Some(&vec!["okta-acme".into()])
        );
        assert!(!got.contains_key("empty"));
        assert!(!got.contains_key("missing"));
        assert!(!got.contains_key("obj"));
    }

    #[test]
    fn extract_scopes_handles_strings_arrays_and_missing() {
        let claims = json!({
            "scope": "project:write project:read project:write",
            "scp": ["b:read", " a:read ", "", "a:read", 7],
            "nested": { "scopes": "org:read" },
            "empty_str": "   ",
            "empty_arr": [],
            "null_val": null,
            "num": 42
        });
        assert_eq!(
            extract_scopes(&claims, &parse_claim_path("scope")),
            Some(vec!["project:read".into(), "project:write".into()])
        );
        assert_eq!(
            extract_scopes(&claims, &parse_claim_path("scp")),
            Some(vec!["a:read".into(), "b:read".into()])
        );
        assert_eq!(
            extract_scopes(&claims, &parse_claim_path("nested.scopes")),
            Some(vec!["org:read".into()])
        );
        assert_eq!(
            extract_scopes(&claims, &parse_claim_path("empty_str")),
            Some(vec![])
        );
        assert_eq!(
            extract_scopes(&claims, &parse_claim_path("empty_arr")),
            Some(vec![])
        );
        assert_eq!(extract_scopes(&claims, &parse_claim_path("missing")), None);
        assert_eq!(extract_scopes(&claims, &parse_claim_path("null_val")), None);
        assert_eq!(extract_scopes(&claims, &parse_claim_path("num")), None);
        assert_eq!(extract_scopes(&claims, &[]), None);
    }

    fn request(authorization: Option<&str>) -> RequestHeader {
        let mut req = RequestHeader::build("GET", b"/x", None).unwrap();
        if let Some(value) = authorization {
            req.insert_header("Authorization", value).unwrap();
        }
        req
    }

    #[tokio::test]
    async fn anything_but_a_jwt_on_bearer_is_a_key_as_sent() {
        let a = authenticator();
        for key in ["plat5-k-2-abc", "sk_live_x", "a.b", "a.b.c.d"] {
            assert_eq!(
                a.present(&request(Some(&format!("Bearer {key}"))), &paths(&["name"]))
                    .await,
                Ok(Presented::Key(key.to_string())),
                "{key}"
            );
        }
        assert!(is_jwt("header.payload.sig") && is_jwt(".."));
    }

    #[tokio::test]
    async fn a_jwt_needs_jwks_and_other_schemes_are_refused() {
        let a = authenticator();
        assert_eq!(
            a.present(&request(None), &paths(&["name"])).await,
            Err(AuthError::MissingAuthorization)
        );
        for value in ["Basic abc", "Bearer", "Bearer a b", "bearer x"] {
            assert_eq!(
                a.present(&request(Some(value)), &paths(&["name"])).await,
                Err(AuthError::InvalidAuthorizationHeader),
                "{value}"
            );
        }
        // The JWKS URL answers nothing: a JWT can't be verified.
        assert_eq!(
            a.present(&request(Some("Bearer h.p.s")), &paths(&["name"]))
                .await,
            Err(AuthError::JwtValidationUnavailable)
        );
        let mut req = request(None);
        req.insert_header("X-API-Key", "plat5-k-2-abc").unwrap();
        assert_eq!(
            a.present(&req, &paths(&["name"])).await,
            Err(AuthError::MissingAuthorization),
            "X-API-Key is not a credential"
        );
    }

    #[test]
    fn first_name_truncates_to_128_chars_and_trims() {
        let long_ascii = format!("  {}  ", "a".repeat(200));
        assert_eq!(
            first_name(&json!({ "name": long_ascii }), &paths(&["name"])),
            Some("a".repeat(128))
        );

        let long_utf8 = "é".repeat(200);
        let got = first_name(&json!({ "name": long_utf8 }), &paths(&["name"])).unwrap();
        assert_eq!(got.chars().count(), 128);
        assert_eq!(got, "é".repeat(128));

        // Space right at the 128-char boundary is trimmed after truncation.
        let space_at_boundary = format!("{} tail", "b".repeat(127));
        assert_eq!(
            first_name(&json!({ "name": space_at_boundary }), &paths(&["name"])),
            Some("b".repeat(127))
        );
    }

    #[tokio::test]
    async fn expired_cached_jwt_is_rejected_and_evicted_immediately() {
        use std::time::{Duration, SystemTime, UNIX_EPOCH};
        let now_secs = SystemTime::now()
            .duration_since(UNIX_EPOCH)
            .unwrap()
            .as_secs();

        let mut a = authenticator();
        a.jwt_cache = JwtCache::with_fixed_ttl(100, Duration::from_secs(60));

        for (tok, exp) in [
            ("h.p.int", json!(now_secs.saturating_sub(1))),
            ("h.p.now", json!(now_secs)),
            ("h.p.flt", json!((now_secs.saturating_sub(1)) as f64)),
        ] {
            a.jwt_cache
                .put(
                    tok,
                    jsonwebtoken::TokenData {
                        header: jsonwebtoken::Header::new(jsonwebtoken::Algorithm::EdDSA),
                        claims: json!({ "sub": "user-1", "exp": exp }),
                    },
                )
                .await;
            assert_eq!(
                a.present(&request(Some(&format!("Bearer {tok}"))), &paths(&["name"]))
                    .await,
                Err(AuthError::InvalidToken {
                    reason: "token_expired"
                }),
                "{tok}"
            );
            assert!(
                a.jwt_cache.get(tok).await.is_none(),
                "{tok} must be evicted on expired hit"
            );
        }
    }
}
