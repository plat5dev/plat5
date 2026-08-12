use std::time::Instant;

use jsonwebtoken::errors::Error as JwtError;
use jsonwebtoken::jwk::JwkSet;
use jsonwebtoken::{decode, decode_header, DecodingKey, TokenData, Validation};
use serde_json::Value;
use tracing::debug;

use crate::metrics;

pub async fn validate_token(
    token: &str,
    issuer: String,
    jwks: JwkSet,
    allowed_audiences: Vec<String>,
) -> Result<(TokenData<Value>, String), JwtError> {
    let token = token.to_string();
    let span = tracing::Span::current();

    tokio::task::spawn_blocking(move || {
        let _guard = span.enter();
        validate_token_sync(&token, &issuer, &jwks, &allowed_audiences)
    })
    .await
    .map_err(|join_err| {
        tracing::warn!(?join_err, "jwt validation task panicked");
        JwtError::from(jsonwebtoken::errors::ErrorKind::InvalidToken)
    })?
}

fn validate_token_sync(
    token: &str,
    issuer: &str,
    jwks: &JwkSet,
    allowed_audiences: &[String],
) -> Result<(TokenData<Value>, String), JwtError> {
    let header = decode_header(token)?;

    let kid = header
        .kid
        .clone()
        .ok_or_else(|| JwtError::from(jsonwebtoken::errors::ErrorKind::InvalidToken))?;

    let jwk = jwks
        .find(&kid)
        .ok_or_else(|| JwtError::from(jsonwebtoken::errors::ErrorKind::InvalidToken))?;
    let alg = header.alg;

    let mut validation = Validation::new(alg);
    validation.set_issuer(&[issuer]);
    if !allowed_audiences.is_empty() {
        validation.set_audience(allowed_audiences);
    } else {
        validation.validate_aud = false;
    }
    validation.set_required_spec_claims(&["exp"]);

    let decoding_key = DecodingKey::from_jwk(jwk)?;

    let start = Instant::now();
    let result = decode::<Value>(token, &decoding_key, &validation);
    let duration = start.elapsed().as_secs_f64();

    match &result {
        Ok(_) => {
            metrics::record_auth_validation("jwt", "ok", duration);
            debug!(kid = %kid, "jwt validated successfully");
        }
        Err(err) => {
            metrics::record_auth_validation("jwt", "error", duration);
            debug!(kid = %kid, ?err, "jwt validation failed");
        }
    }

    result.map(|token_data| (token_data, kid))
}
