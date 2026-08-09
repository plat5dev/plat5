use serde_json::Value;

/// Authentication context extracted from a successful auth check.
pub struct AuthContext {
    pub user_id: String,
    pub auth_type: &'static str,
    pub kid: Option<String>,
}

pub enum ResolveDeny {
    NotFound,
    Unavailable,
}

#[derive(Debug)]
pub enum AuthError {
    MissingAuthorization,
    InvalidAuthorizationHeader,
    InvalidToken { reason: &'static str },
    MissingUserId,
    MissingApiKey,
    InvalidApiKeyHeader,
    InvalidApiKey,
    JwtValidationUnavailable,
    ApiKeyValidationUnavailable,
}

impl AuthError {
    pub fn as_str(&self) -> &'static str {
        match self {
            AuthError::MissingAuthorization => "missing_authorization",
            AuthError::InvalidAuthorizationHeader => "invalid_authorization",
            AuthError::InvalidToken { reason } => reason,
            AuthError::MissingUserId => "missing_user_id",
            AuthError::MissingApiKey => "missing_apikey",
            AuthError::InvalidApiKeyHeader => "invalid_apikey_header",
            AuthError::InvalidApiKey => "invalid_apikey",
            AuthError::JwtValidationUnavailable => "jwks_unavailable",
            AuthError::ApiKeyValidationUnavailable => "apikey_service_unavailable",
        }
    }

    pub fn auth_type(&self) -> &'static str {
        match self {
            AuthError::MissingAuthorization
            | AuthError::InvalidAuthorizationHeader
            | AuthError::InvalidToken { .. }
            | AuthError::MissingUserId
            | AuthError::JwtValidationUnavailable => "jwt",
            AuthError::MissingApiKey
            | AuthError::InvalidApiKeyHeader
            | AuthError::InvalidApiKey
            | AuthError::ApiKeyValidationUnavailable => "apikey",
        }
    }

    pub fn is_client_error(&self) -> bool {
        matches!(
            self,
            AuthError::MissingAuthorization
                | AuthError::InvalidAuthorizationHeader
                | AuthError::InvalidToken { .. }
                | AuthError::MissingUserId
                | AuthError::MissingApiKey
                | AuthError::InvalidApiKeyHeader
                | AuthError::InvalidApiKey
        )
    }

    pub fn details(&self) -> Option<serde_json::Value> {
        match self {
            AuthError::InvalidToken { reason } => Some(serde_json::json!({ "reason": *reason })),
            _ => None,
        }
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

/// Parse `AUTH_USER_ID_CLAIM` into path segments. Empty/invalid → default OpenAuth path.
pub fn parse_user_id_claim(raw: &str) -> Vec<String> {
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

/// Organization id from path params for an org-scoped route.
pub fn organization_id_from_params(
    organization_param: Option<&str>,
    params: &std::collections::HashMap<String, String>,
) -> Result<String, OrgParamError> {
    let param_name = match organization_param {
        Some(p) if !p.is_empty() => p,
        _ => return Err(OrgParamError::MissingParamName),
    };
    match params.get(param_name) {
        Some(id) if !id.is_empty() => Ok(id.clone()),
        _ => Err(OrgParamError::MissingParamValue {
            param: param_name.to_string(),
        }),
    }
}

#[derive(Debug)]
pub enum OrgParamError {
    MissingParamName,
    MissingParamValue { param: String },
}

#[cfg(test)]
mod tests {
    use super::*;
    use serde_json::json;
    use std::collections::HashMap;

    #[test]
    fn parse_default_and_sub() {
        assert_eq!(
            parse_user_id_claim("properties.user_id"),
            vec!["properties", "user_id"]
        );
        assert_eq!(parse_user_id_claim("sub"), vec!["sub"]);
        assert_eq!(parse_user_id_claim(""), vec!["properties", "user_id"]);
    }

    #[test]
    fn extract_nested_and_sub() {
        let claims = json!({"properties": {"user_id": "01ABC"}, "sub": "auth0|1"});
        assert_eq!(
            extract_claim_path(&claims, &parse_user_id_claim("properties.user_id")).as_deref(),
            Some("01ABC")
        );
        assert_eq!(
            extract_claim_path(&claims, &parse_user_id_claim("sub")).as_deref(),
            Some("auth0|1")
        );
        assert!(extract_claim_path(&claims, &parse_user_id_claim("missing")).is_none());
    }

    #[test]
    fn org_id_from_params() {
        let mut params = HashMap::new();
        params.insert("organization_id".into(), "org_1".into());
        assert_eq!(
            organization_id_from_params(Some("organization_id"), &params).unwrap(),
            "org_1"
        );
        assert!(matches!(
            organization_id_from_params(None, &params),
            Err(OrgParamError::MissingParamName)
        ));
    }
}
