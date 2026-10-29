//! Who presented a request: the `Authorization: Bearer` value, as a verified JWT's
//! issuer and subject or as a key (docs/concepts/access.md#credentials). Admit then
//! says whose it is.

pub mod admit;
pub mod call;
pub mod jwt;

use pingora::http::RequestHeader;
use serde_json::Value;

use self::jwt::{validate_token, JwtCache, JwtValidatorState};

const JWT_CACHE_CAPACITY: u64 = 10_000;
const JWT_CACHE_TTL_BUFFER_SECS: u64 = 60;

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
}

/// `users.name.claims`, each as path segments, in order.
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
}

impl Authenticator {
    pub fn new(jwt_validator: JwtValidatorState, subject_claim: Vec<String>) -> Self {
        Self {
            jwt_validator,
            jwt_cache: JwtCache::new(JWT_CACHE_CAPACITY, JWT_CACHE_TTL_BUFFER_SECS),
            subject_claim,
        }
    }

    /// The request's credential, from `Authorization: Bearer`. `name_claims` is
    /// `users.name.claims`: a JWT's name is the first of them that's non-empty.
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
        Ok(Presented::Jwt {
            // Validation checked `iss` against the configured issuer.
            issuer: self.jwt_validator.get_issuer(),
            subject,
            name: first_name(&claims, name_claims),
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

/// The first of `paths` that holds a string with something besides spaces,
/// trimmed: the person's name (docs/reference/idp.md#a-persons-name).
pub fn first_name(claims: &Value, paths: &[Vec<String>]) -> Option<String> {
    paths.iter().find_map(|path| {
        let name = extract_claim_path(claims, path)?;
        let name = name.trim();
        (!name.is_empty()).then(|| name.to_string())
    })
}

/// A dotted claim path (`AUTH_SUBJECT_CLAIM`, each of `users.name.claims`) as
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
        )
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
}
