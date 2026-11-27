use std::sync::Arc;
use std::time::Instant;

use jsonwebtoken::errors::{Error as JwtError, ErrorKind};
use jsonwebtoken::jwk::{AlgorithmParameters, JwkSet, KeyAlgorithm, PublicKeyUse};
use jsonwebtoken::{decode, decode_header, Algorithm, DecodingKey, TokenData, Validation};
use serde_json::Value;
use tracing::debug;

use super::Audiences;
use crate::metrics;

pub fn extract_kid(token: &str) -> Option<String> {
    decode_header(token).ok().and_then(|h| h.kid)
}

pub async fn validate_token(
    token: &str,
    issuer: String,
    jwks: Arc<JwkSet>,
    allowed_audiences: Audiences,
) -> Result<(TokenData<Value>, String), JwtError> {
    let token = token.to_string();
    let span = tracing::Span::current();

    tokio::task::spawn_blocking(move || {
        let _guard = span.enter();
        validate_token_sync(&token, &issuer, &jwks, &allowed_audiences)
    })
    .await
    .map_err(|join_err| {
        tracing::warn!(error = ?join_err, "jwt validation task panicked");
        JwtError::from(ErrorKind::InvalidToken)
    })?
}

fn validate_token_sync(
    token: &str,
    issuer: &str,
    jwks: &JwkSet,
    allowed_audiences: &Audiences,
) -> Result<(TokenData<Value>, String), JwtError> {
    let header = decode_header(token)?;

    let kid = header
        .kid
        .clone()
        .ok_or_else(|| JwtError::from(ErrorKind::InvalidToken))?;

    let jwk = jwks
        .find(&kid)
        .ok_or_else(|| JwtError::from(ErrorKind::InvalidToken))?;
    let alg = header.alg;

    if matches!(jwk.common.public_key_use, Some(PublicKeyUse::Encryption)) {
        return Err(JwtError::from(ErrorKind::InvalidAlgorithm));
    }
    if matches!(jwk.algorithm, AlgorithmParameters::OctetKey(_))
        || matches!(alg, Algorithm::HS256 | Algorithm::HS384 | Algorithm::HS512)
    {
        return Err(JwtError::from(ErrorKind::InvalidAlgorithm));
    }
    if let Some(kalg) = jwk.common.key_algorithm {
        if !key_algorithm_matches(kalg, alg) {
            return Err(JwtError::from(ErrorKind::InvalidAlgorithm));
        }
    }

    let mut validation = Validation::new(alg);
    validation.set_issuer(&[issuer]);
    match allowed_audiences {
        Audiences::OneOf(list) => validation.set_audience(list),
        Audiences::Any => validation.validate_aud = false,
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
            debug!(kid = %kid, error = ?err, "jwt validation failed");
        }
    }

    result.map(|token_data| (token_data, kid))
}

fn key_algorithm_matches(kalg: KeyAlgorithm, alg: Algorithm) -> bool {
    matches!(
        (kalg, alg),
        (KeyAlgorithm::ES256, Algorithm::ES256)
            | (KeyAlgorithm::ES384, Algorithm::ES384)
            | (KeyAlgorithm::RS256, Algorithm::RS256)
            | (KeyAlgorithm::RS384, Algorithm::RS384)
            | (KeyAlgorithm::RS512, Algorithm::RS512)
            | (KeyAlgorithm::PS256, Algorithm::PS256)
            | (KeyAlgorithm::PS384, Algorithm::PS384)
            | (KeyAlgorithm::PS512, Algorithm::PS512)
            | (KeyAlgorithm::EdDSA, Algorithm::EdDSA)
    )
}

#[cfg(test)]
mod tests {
    use super::*;
    use jsonwebtoken::{encode, EncodingKey, Header};
    use serde_json::json;
    use std::time::{SystemTime, UNIX_EPOCH};

    const ED25519_PRIVATE_DER: &[u8] = &[
        0x30, 0x2e, 0x02, 0x01, 0x00, 0x30, 0x05, 0x06, 0x03, 0x2b, 0x65, 0x70, 0x04, 0x22, 0x04,
        0x20, 0xbf, 0xb5, 0xcd, 0x6d, 0x82, 0xec, 0x0e, 0x21, 0x15, 0xe7, 0x6b, 0x03, 0x48, 0x76,
        0xd0, 0x90, 0xf4, 0x3e, 0xb5, 0x28, 0x01, 0x8f, 0x36, 0x01, 0xf8, 0xae, 0xf9, 0x5d, 0xac,
        0xc5, 0xa1, 0xef,
    ];
    const ED25519_PUBLIC_X: &str = "bXUj6dVFBJp4oH4PZBzpnTQAuK8oEtKDiftREsbKRFI";
    const ISSUER: &str = "https://auth.example.com";

    fn future_exp() -> u64 {
        SystemTime::now()
            .duration_since(UNIX_EPOCH)
            .unwrap()
            .as_secs()
            + 300
    }

    fn claims() -> Value {
        json!({
            "iss": ISSUER,
            "sub": "user-1",
            "aud": "api",
            "exp": future_exp(),
        })
    }

    fn sign_eddsa(kid: &str) -> String {
        let mut header = Header::new(Algorithm::EdDSA);
        header.kid = Some(kid.to_string());
        let key = EncodingKey::from_ed_der(ED25519_PRIVATE_DER);
        encode(&header, &claims(), &key).unwrap()
    }

    fn sign_hs256(kid: &str, secret: &[u8]) -> String {
        let mut header = Header::new(Algorithm::HS256);
        header.kid = Some(kid.to_string());
        let key = EncodingKey::from_secret(secret);
        encode(&header, &claims(), &key).unwrap()
    }

    fn jwks(value: Value) -> JwkSet {
        serde_json::from_value(value).unwrap()
    }

    #[test]
    fn valid_asymmetric_token_is_accepted_with_matching_or_omitted_use_and_alg() {
        let token = sign_eddsa("k1");
        let with_use_and_alg = jwks(json!({
            "keys": [{
                "kty": "OKP",
                "crv": "Ed25519",
                "kid": "k1",
                "use": "sig",
                "alg": "EdDSA",
                "x": ED25519_PUBLIC_X
            }]
        }));
        let (data, kid) =
            validate_token_sync(&token, ISSUER, &with_use_and_alg, &Audiences::Any).unwrap();
        assert_eq!(kid, "k1");
        assert_eq!(data.claims["sub"], "user-1");

        let without_optional_fields = jwks(json!({
            "keys": [{
                "kty": "OKP",
                "crv": "Ed25519",
                "kid": "k1",
                "x": ED25519_PUBLIC_X
            }]
        }));
        assert!(
            validate_token_sync(&token, ISSUER, &without_optional_fields, &Audiences::Any).is_ok()
        );
    }

    #[test]
    fn rejects_encryption_jwk_use() {
        let token = sign_eddsa("k1");
        let set = jwks(json!({
            "keys": [{
                "kty": "OKP",
                "crv": "Ed25519",
                "kid": "k1",
                "use": "enc",
                "alg": "EdDSA",
                "x": ED25519_PUBLIC_X
            }]
        }));
        let err = validate_token_sync(&token, ISSUER, &set, &Audiences::Any).unwrap_err();
        assert_eq!(*err.kind(), ErrorKind::InvalidAlgorithm);
    }

    #[test]
    fn rejects_symmetric_hmac_keys_and_algorithms() {
        // Symmetric JWK ("kty": "oct") with matching valid HS256 signature must be rejected.
        let token = sign_hs256("k-oct", b"super-secret");
        let oct_set = jwks(json!({
            "keys": [{
                "kty": "oct",
                "kid": "k-oct",
                "alg": "HS256",
                "k": "c3VwZXItc2VjcmV0"
            }]
        }));
        let err = validate_token_sync(&token, ISSUER, &oct_set, &Audiences::Any).unwrap_err();
        assert_eq!(*err.kind(), ErrorKind::InvalidAlgorithm);

        // HS256 header referencing an asymmetric key without JWK alg must also be rejected.
        let confused_token = sign_hs256("k1", b"anything");
        let asym_set = jwks(json!({
            "keys": [{
                "kty": "OKP",
                "crv": "Ed25519",
                "kid": "k1",
                "x": ED25519_PUBLIC_X
            }]
        }));
        let err =
            validate_token_sync(&confused_token, ISSUER, &asym_set, &Audiences::Any).unwrap_err();
        assert_eq!(*err.kind(), ErrorKind::InvalidAlgorithm);
    }

    #[test]
    fn rejects_mismatched_jwk_alg() {
        let token = sign_eddsa("k1");
        let set = jwks(json!({
            "keys": [{
                "kty": "OKP",
                "crv": "Ed25519",
                "kid": "k1",
                "use": "sig",
                "alg": "ES256",
                "x": ED25519_PUBLIC_X
            }]
        }));
        let err = validate_token_sync(&token, ISSUER, &set, &Audiences::Any).unwrap_err();
        assert_eq!(*err.kind(), ErrorKind::InvalidAlgorithm);
    }
}
