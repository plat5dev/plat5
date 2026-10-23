//! Who presented a request: an `X-API-Key`, or a verified JWT's issuer and subject
//! (docs/concepts/access.md#credentials). Admit then says whose it is.

pub mod admit;
pub mod call;
pub mod jwt;
pub mod shape;

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
    /// `X-API-Key`, as sent. Its shape is not checked yet.
    Key(String),
    /// A verified JWT.
    Jwt {
        token: String,
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
    /// An `X-API-Key` that isn't `{brand}-k-2-{secret}`, or that admit doesn't accept.
    InvalidKey,
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
            AuthError::InvalidKey => "invalid_credential",
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

    /// The request's credential. `X-API-Key` present means it is the credential,
    /// including an empty value; don't fall through to `Authorization`.
    /// `name_claim` is the sign-in kind's.
    pub async fn present(
        &self,
        req: &RequestHeader,
        name_claim: &str,
    ) -> Result<Presented, AuthError> {
        if let Some(value) = req.headers.get("X-API-Key") {
            let key = value.to_str().map_err(|_| AuthError::InvalidKey)?;
            return Ok(Presented::Key(key.to_string()));
        }
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
                    self.jwt_validator.get_allowed_audiences().to_vec(),
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
        let name_path: Vec<String> = vec![name_claim.to_string()];
        Ok(Presented::Jwt {
            token: token.to_string(),
            // Validation checked `iss` against the configured issuer.
            issuer: self.jwt_validator.get_issuer(),
            subject,
            name: extract_claim_path(&claims, &name_path),
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

/// Walk a dotted path into JWT claims JSON (e.g. `properties.user_id`, `sub`).
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

/// Parse `AUTH_SUBJECT_CLAIM` into path segments. Empty → the default, `properties.user_id`.
pub fn parse_subject_claim(raw: &str) -> Vec<String> {
    let parts: Vec<String> = raw
        .split('.')
        .map(|s| s.trim().to_string())
        .filter(|s| !s.is_empty())
        .collect();
    if parts.is_empty() {
        vec!["properties".to_string(), "user_id".to_string()]
    } else {
        parts
    }
}

#[cfg(test)]
mod tests {
    use super::*;
    use serde_json::json;

    #[test]
    fn parse_default_and_sub() {
        assert_eq!(
            parse_subject_claim("properties.user_id"),
            vec!["properties", "user_id"]
        );
        assert_eq!(parse_subject_claim("sub"), vec!["sub"]);
        assert_eq!(parse_subject_claim(""), vec!["properties", "user_id"]);
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

    fn authenticator() -> Authenticator {
        Authenticator::new(
            JwtValidatorState::new(
                "https://auth".into(),
                "http://127.0.0.1:1/jwks".into(),
                vec![],
            ),
            parse_subject_claim("sub"),
        )
    }

    #[tokio::test]
    async fn the_api_key_header_wins_and_never_falls_through() {
        let a = authenticator();
        let mut req = RequestHeader::build("GET", b"/x", None).unwrap();
        req.insert_header("Authorization", "Bearer x.y.z").unwrap();
        req.insert_header("X-API-Key", "").unwrap();
        assert_eq!(
            a.present(&req, "name").await,
            Ok(Presented::Key(String::new()))
        );

        let req = RequestHeader::build("GET", b"/x", None).unwrap();
        assert_eq!(
            a.present(&req, "name").await,
            Err(AuthError::MissingAuthorization)
        );

        let mut req = RequestHeader::build("GET", b"/x", None).unwrap();
        req.insert_header("Authorization", "Basic abc").unwrap();
        assert_eq!(
            a.present(&req, "name").await,
            Err(AuthError::InvalidAuthorizationHeader)
        );
    }
}
