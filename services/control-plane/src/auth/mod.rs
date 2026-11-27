pub mod config;
pub mod jwks;
#[cfg(test)]
mod tests;

use std::collections::HashMap;
use std::sync::Arc;

use base64::engine::general_purpose::URL_SAFE_NO_PAD;
use base64::Engine;
use jsonwebtoken::errors::ErrorKind;
use jsonwebtoken::jwk::{AlgorithmParameters, JwkSet, KeyAlgorithm, PublicKeyUse};
use jsonwebtoken::{decode, decode_header, Algorithm, DecodingKey, Validation};
use serde::Serialize;
use serde_json::{json, Value};
use subtle::ConstantTimeEq;
use tokio::sync::watch;

pub use config::{AccessLevel, AuthConfig, AuthRule, CliAuthConfig};
use config::{claim_at, parse_duration_secs, rule_matches};
use jwks::IssuerJwks;

const CLOCK_SKEW_SECS: u64 = 60;
const MAX_SUBJECT_LEN: usize = 256;

/// Cryptographically verified caller identity attached by auth middleware.
#[derive(Debug, Clone, PartialEq, Eq, Serialize)]
pub struct AuthenticatedActor {
    #[serde(rename = "type")]
    pub actor_type: &'static str,
    #[serde(skip_serializing_if = "Option::is_none")]
    pub issuer: Option<String>,
    #[serde(skip_serializing_if = "Option::is_none")]
    pub issuer_name: Option<String>,
    #[serde(skip_serializing_if = "Option::is_none")]
    pub subject: Option<String>,
    #[serde(skip)]
    pub access: AccessLevel,
}

impl AuthenticatedActor {
    pub fn admin_token() -> Self {
        Self {
            actor_type: "admin_token",
            issuer: None,
            issuer_name: None,
            subject: None,
            access: AccessLevel::Apply,
        }
    }

    pub fn jwt(
        issuer_name: String,
        issuer: String,
        subject: String,
        access: AccessLevel,
    ) -> Self {
        Self {
            actor_type: "jwt",
            issuer: Some(issuer),
            issuer_name: Some(issuer_name),
            subject: Some(subject),
            access,
        }
    }
}

#[derive(Debug, Clone, PartialEq, Eq)]
pub enum AuthFailure {
    Unauthorized {
        reason: &'static str,
    },
    Forbidden {
        reason: &'static str,
        required: AccessLevel,
    },
    Unavailable {
        reason: &'static str,
    },
}

impl AuthFailure {
    pub fn reason(&self) -> &'static str {
        match self {
            AuthFailure::Unauthorized { reason }
            | AuthFailure::Forbidden { reason, .. }
            | AuthFailure::Unavailable { reason } => reason,
        }
    }
}

#[derive(Clone)]
struct IssuerVerifier {
    name: String,
    issuer: String,
    audiences: Vec<String>,
    subject_claim: String,
    max_token_lifetime_secs: Option<u64>,
    cli: Option<CliAuthConfig>,
    rules: Vec<AuthRule>,
    jwks: IssuerJwks,
}

#[derive(Clone)]
pub struct Authenticator {
    admin_token: Option<Arc<str>>,
    issuers: Arc<Vec<IssuerVerifier>>,
    by_issuer: Arc<HashMap<String, usize>>,
}

impl Authenticator {
    pub fn new(admin_token: Option<String>, config: Option<AuthConfig>) -> Self {
        let mut verifiers = Vec::new();
        let mut by_issuer = HashMap::new();
        if let Some(cfg) = config {
            for (idx, iss) in cfg.issuers.into_iter().enumerate() {
                let max_lifetime = iss
                    .max_token_lifetime
                    .as_deref()
                    .and_then(parse_duration_secs);
                let jwks = IssuerJwks::new(iss.issuer.clone(), iss.jwks_uri);
                by_issuer.insert(iss.issuer.clone(), idx);
                verifiers.push(IssuerVerifier {
                    name: iss.name,
                    issuer: iss.issuer,
                    audiences: iss.audiences,
                    subject_claim: iss.subject_claim,
                    max_token_lifetime_secs: max_lifetime,
                    cli: iss.cli,
                    rules: iss.rules,
                    jwks,
                });
            }
        }
        Self {
            admin_token: admin_token.map(Into::into),
            issuers: Arc::new(verifiers),
            by_issuer: Arc::new(by_issuer),
        }
    }

    #[cfg(test)]
    pub fn static_token(token: impl Into<String>) -> Self {
        Self::new(Some(token.into()), None)
    }

    #[cfg(test)]
    pub fn with_static_jwks(
        admin_token: Option<String>,
        config: AuthConfig,
        jwks_by_issuer: HashMap<String, JwkSet>,
    ) -> Self {
        let mut verifiers = Vec::new();
        let mut by_issuer = HashMap::new();
        for (idx, iss) in config.issuers.into_iter().enumerate() {
            let max_lifetime = iss
                .max_token_lifetime
                .as_deref()
                .and_then(parse_duration_secs);
            let jwks = match jwks_by_issuer.get(&iss.issuer) {
                Some(set) => IssuerJwks::with_static_jwks(iss.issuer.clone(), set.clone()),
                None => IssuerJwks::new(iss.issuer.clone(), iss.jwks_uri),
            };
            by_issuer.insert(iss.issuer.clone(), idx);
            verifiers.push(IssuerVerifier {
                name: iss.name,
                issuer: iss.issuer,
                audiences: iss.audiences,
                subject_claim: iss.subject_claim,
                max_token_lifetime_secs: max_lifetime,
                cli: iss.cli,
                rules: iss.rules,
                jwks,
            });
        }
        Self {
            admin_token: admin_token.map(Into::into),
            issuers: Arc::new(verifiers),
            by_issuer: Arc::new(by_issuer),
        }
    }

    pub fn has_admin_token(&self) -> bool {
        self.admin_token.is_some()
    }

    pub fn has_oidc(&self) -> bool {
        !self.issuers.is_empty()
    }

    pub fn spawn_refresh(&self, shutdown: watch::Receiver<bool>) {
        for iss in self.issuers.iter() {
            iss.jwks.spawn_refresh(shutdown.clone());
        }
    }

    /// True when no OIDC issuers are configured, or every configured issuer
    /// has successfully fetched its JWKS at least once.
    pub fn is_ready(&self) -> bool {
        self.issuers.iter().all(|i| i.jwks.is_ready())
    }

    /// Public, non-secret metadata for `GET /auth/info` (used by `plat5 login`).
    pub fn auth_info(&self) -> Value {
        let mut methods = Vec::new();
        if self.has_admin_token() {
            methods.push("admin_token");
        }
        if self.has_oidc() {
            methods.push("oidc");
        }
        let issuers: Vec<Value> = self
            .issuers
            .iter()
            .map(|i| {
                let mut obj = json!({
                    "name": i.name,
                    "issuer": i.issuer,
                    "audiences": i.audiences,
                });
                if let Some(ref cli) = i.cli {
                    obj["cli"] = serde_json::to_value(cli).expect("cli serializes");
                }
                obj
            })
            .collect();
        json!({
            "methods": methods,
            "issuers": issuers,
        })
    }

    pub async fn authenticate(
        &self,
        auth_header: Option<&str>,
        required: AccessLevel,
    ) -> Result<AuthenticatedActor, AuthFailure> {
        let Some(token) = auth_header.and_then(|v| v.strip_prefix("Bearer ")) else {
            return Err(AuthFailure::Unauthorized {
                reason: "invalid_or_missing_credential",
            });
        };
        if token.is_empty() {
            return Err(AuthFailure::Unauthorized {
                reason: "invalid_or_missing_credential",
            });
        }

        // 1. Static ADMIN_TOKEN (constant-time comparison).
        if let Some(ref expected) = self.admin_token {
            if bool::from(token.as_bytes().ct_eq(expected.as_bytes())) {
                return Ok(AuthenticatedActor::admin_token());
            }
        }

        // 2. Multi-issuer OIDC JWT verification.
        if self.issuers.is_empty() {
            return Err(AuthFailure::Unauthorized {
                reason: "invalid_or_missing_admin_token",
            });
        }

        self.verify_jwt(token, required).await
    }

    async fn verify_jwt(
        &self,
        token: &str,
        required: AccessLevel,
    ) -> Result<AuthenticatedActor, AuthFailure> {
        // Peek unverified `iss` only to select which trusted issuer's JWKS and
        // validation parameters to verify against.
        let unverified_iss = peek_issuer(token).ok_or(AuthFailure::Unauthorized {
            reason: "malformed_jwt",
        })?;
        let Some(&idx) = self.by_issuer.get(&unverified_iss) else {
            return Err(AuthFailure::Unauthorized {
                reason: "unknown_issuer",
            });
        };
        let verifier = &self.issuers[idx];

        let header = decode_header(token).map_err(|_| AuthFailure::Unauthorized {
            reason: "malformed_jwt",
        })?;
        let Some(kid) = header.kid.clone().filter(|k| !k.is_empty()) else {
            return Err(AuthFailure::Unauthorized {
                reason: "missing_kid",
            });
        };

        let mut jwks = verifier
            .jwks
            .get_jwks()
            .await
            .map_err(|_| AuthFailure::Unavailable {
                reason: "jwks_unavailable",
            })?;

        if jwks.find(&kid).is_none() {
            if let Some(refreshed) = verifier.jwks.refresh_for_unknown_kid(&kid).await {
                jwks = refreshed;
            }
        }
        if jwks.find(&kid).is_none() {
            return Err(AuthFailure::Unauthorized {
                reason: "unknown_kid",
            });
        }

        let token_owned = token.to_string();
        let issuer_owned = verifier.issuer.clone();
        let audiences_owned = verifier.audiences.clone();
        let jwks_owned = Arc::clone(&jwks);

        let claims = tokio::task::spawn_blocking(move || {
            validate_jwt_sync(&token_owned, &issuer_owned, &audiences_owned, &jwks_owned)
        })
        .await
        .map_err(|_| AuthFailure::Unauthorized {
            reason: "invalid_jwt",
        })??;

        // Optional max token lifetime (exp - iat).
        if let Some(max_secs) = verifier.max_token_lifetime_secs {
            let iat = claims.get("iat").and_then(Value::as_u64);
            let exp = claims.get("exp").and_then(Value::as_u64);
            match (iat, exp) {
                (Some(iat), Some(exp)) if exp > iat && exp - iat <= max_secs => {}
                _ => {
                    return Err(AuthFailure::Unauthorized {
                        reason: "token_lifetime_exceeded",
                    });
                }
            }
        }

        // Required non-empty string subject at configured claim path.
        let subject = claim_at(&claims, &verifier.subject_claim)
            .and_then(Value::as_str)
            .map(str::trim)
            .filter(|s| !s.is_empty() && s.len() <= MAX_SUBJECT_LEN)
            .ok_or(AuthFailure::Unauthorized {
                reason: "missing_subject",
            })?
            .to_string();

        // Evaluate rules (OR across rules, AND across claims in a rule).
        let granted = verifier
            .rules
            .iter()
            .filter(|r| rule_matches(r, &claims))
            .map(|r| r.access)
            .max();

        let Some(granted) = granted else {
            return Err(AuthFailure::Forbidden {
                reason: "claims_not_matched",
                required,
            });
        };

        if granted < required {
            return Err(AuthFailure::Forbidden {
                reason: "insufficient_access",
                required,
            });
        }

        Ok(AuthenticatedActor::jwt(
            verifier.name.clone(),
            verifier.issuer.clone(),
            subject,
            granted,
        ))
    }
}

fn peek_issuer(token: &str) -> Option<String> {
    let mut parts = token.split('.');
    let _header = parts.next()?;
    let payload = parts.next()?;
    let _sig = parts.next()?;
    if parts.next().is_some() {
        return None;
    }
    let decoded = URL_SAFE_NO_PAD.decode(payload).ok()?;
    let obj: Value = serde_json::from_slice(&decoded).ok()?;
    obj.get("iss")
        .and_then(Value::as_str)
        .filter(|s| !s.is_empty())
        .map(str::to_string)
}

fn validate_jwt_sync(
    token: &str,
    issuer: &str,
    audiences: &[String],
    jwks: &JwkSet,
) -> Result<Value, AuthFailure> {
    let header = decode_header(token).map_err(|_| AuthFailure::Unauthorized {
        reason: "malformed_jwt",
    })?;
    let kid = header.kid.ok_or(AuthFailure::Unauthorized {
        reason: "missing_kid",
    })?;
    let jwk = jwks.find(&kid).ok_or(AuthFailure::Unauthorized {
        reason: "unknown_kid",
    })?;
    let alg = header.alg;

    // RFC 8725: strictly asymmetric signing algorithms, reject enc keys and HS*.
    if !is_allowed_asymmetric_alg(alg) {
        return Err(AuthFailure::Unauthorized {
            reason: "invalid_algorithm",
        });
    }
    if matches!(jwk.common.public_key_use, Some(PublicKeyUse::Encryption))
        || matches!(jwk.algorithm, AlgorithmParameters::OctetKey(_))
    {
        return Err(AuthFailure::Unauthorized {
            reason: "invalid_algorithm",
        });
    }
    if let Some(kalg) = jwk.common.key_algorithm {
        if !key_algorithm_matches(kalg, alg) {
            return Err(AuthFailure::Unauthorized {
                reason: "invalid_algorithm",
            });
        }
    }

    let mut validation = Validation::new(alg);
    validation.set_issuer(&[issuer]);
    validation.set_audience(audiences);
    validation.validate_exp = true;
    validation.validate_nbf = true;
    validation.leeway = CLOCK_SKEW_SECS;
    validation.set_required_spec_claims(&["exp", "iss", "aud"]);

    let decoding_key = DecodingKey::from_jwk(jwk).map_err(|_| AuthFailure::Unauthorized {
        reason: "invalid_jwk",
    })?;

    let data = decode::<Value>(token, &decoding_key, &validation).map_err(|err| {
        let reason = match err.kind() {
            ErrorKind::ExpiredSignature => "token_expired",
            ErrorKind::ImmatureSignature => "token_not_yet_valid",
            ErrorKind::InvalidAudience => "invalid_audience",
            ErrorKind::InvalidIssuer => "invalid_issuer",
            ErrorKind::InvalidSignature => "invalid_signature",
            ErrorKind::InvalidAlgorithm => "invalid_algorithm",
            _ => "invalid_jwt",
        };
        AuthFailure::Unauthorized { reason }
    })?;

    Ok(data.claims)
}

fn is_allowed_asymmetric_alg(alg: Algorithm) -> bool {
    matches!(
        alg,
        Algorithm::RS256
            | Algorithm::RS384
            | Algorithm::RS512
            | Algorithm::PS256
            | Algorithm::PS384
            | Algorithm::PS512
            | Algorithm::ES256
            | Algorithm::ES384
            | Algorithm::EdDSA
    )
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
