use std::fmt;
use std::time::{SystemTime, UNIX_EPOCH};

use base64::engine::general_purpose::URL_SAFE_NO_PAD;
use base64::Engine;
use ed25519_dalek::{Signer, SigningKey};
use sha2::{Digest, Sha256};

pub const SIGNATURE_HEADER: &str = "X-Plat5-Signature";
const SIGNATURE_VERSION: &str = "v1";

/// Ed25519 signer for outgoing upstream requests (`X-Plat5-Signature`,
/// docs/reference/headers.md#upstream-request-signing).
#[derive(Clone)]
pub struct UpstreamSigner {
    key: SigningKey,
    kid: String,
}

impl fmt::Debug for UpstreamSigner {
    fn fmt(&self, f: &mut fmt::Formatter<'_>) -> fmt::Result {
        f.debug_struct("UpstreamSigner")
            .field("kid", &self.kid)
            .finish_non_exhaustive()
    }
}

impl UpstreamSigner {
    /// Construct from a base64url-no-pad 32-byte Ed25519 seed and optional `kid`.
    /// When `kid` is `None`, derives the first 16 lowercase hex chars of
    /// `SHA-256(public_key)`.
    pub fn from_seed_b64url(seed_b64url: &str, kid: Option<&str>) -> Result<Self, String> {
        let bytes = URL_SAFE_NO_PAD
            .decode(seed_b64url.trim())
            .map_err(|e| format!("must be base64url without padding: {e}"))?;
        let seed: [u8; 32] = bytes
            .try_into()
            .map_err(|v: Vec<u8>| format!("must decode to 32 bytes, got {}", v.len()))?;
        let key = SigningKey::from_bytes(&seed);
        let kid = match kid {
            Some(raw) => {
                let k = raw.trim();
                validate_kid(k)?;
                k.to_string()
            }
            None => default_kid(&key),
        };
        Ok(Self { key, kid })
    }

    pub fn kid(&self) -> &str {
        &self.kid
    }

    /// Build the `X-Plat5-Signature` header value using the current wall clock.
    pub fn sign_now<'a>(
        &self,
        method: &str,
        path_and_query: &str,
        request_id: &str,
        plat5_headers: impl IntoIterator<Item = (&'a str, &'a str)>,
    ) -> String {
        let now = SystemTime::now()
            .duration_since(UNIX_EPOCH)
            .map(|d| d.as_secs())
            .unwrap_or(0);
        self.sign(method, path_and_query, request_id, plat5_headers, now)
    }

    /// Build the `X-Plat5-Signature` header value for an explicit UTC timestamp.
    pub fn sign<'a>(
        &self,
        method: &str,
        path_and_query: &str,
        request_id: &str,
        plat5_headers: impl IntoIterator<Item = (&'a str, &'a str)>,
        timestamp: u64,
    ) -> String {
        let payload = canonical_payload(
            &self.kid,
            timestamp,
            method,
            path_and_query,
            request_id,
            plat5_headers,
        );
        let sig = self.key.sign(payload.as_bytes());
        let encoded = URL_SAFE_NO_PAD.encode(sig.to_bytes());
        format!("{SIGNATURE_VERSION},kid={},t={timestamp},sig={encoded}", self.kid)
    }
}

/// Canonical payload (UTF-8, `\n`-delimited):
/// ```text
/// v1\n
/// <kid>\n
/// <t>\n
/// <METHOD>\n
/// <path_and_query>\n
/// <X-Request-ID>\n
/// <sorted x-plat5-* headers as lowercase-name:value\n>
/// ```
pub fn canonical_payload<'a>(
    kid: &str,
    timestamp: u64,
    method: &str,
    path_and_query: &str,
    request_id: &str,
    plat5_headers: impl IntoIterator<Item = (&'a str, &'a str)>,
) -> String {
    let mut headers: Vec<(String, &str)> = plat5_headers
        .into_iter()
        .map(|(name, value)| (name.to_ascii_lowercase(), value))
        .filter(|(name, _)| name.starts_with("x-plat5-") && name != "x-plat5-signature")
        .collect();
    headers.sort_by(|a, b| a.0.cmp(&b.0).then_with(|| a.1.cmp(b.1)));

    let mut out = String::with_capacity(128 + headers.len() * 48);
    out.push_str(SIGNATURE_VERSION);
    out.push('\n');
    out.push_str(kid);
    out.push('\n');
    out.push_str(&timestamp.to_string());
    out.push('\n');
    out.push_str(&method.to_ascii_uppercase());
    out.push('\n');
    out.push_str(path_and_query);
    out.push('\n');
    out.push_str(request_id);
    out.push('\n');
    for (name, value) in headers {
        out.push_str(&name);
        out.push(':');
        out.push_str(value);
        out.push('\n');
    }
    out
}

pub fn validate_kid(kid: &str) -> Result<(), String> {
    if kid.is_empty() || kid.len() > 64 {
        return Err("must be 1 to 64 characters".to_string());
    }
    if !kid
        .bytes()
        .all(|b| b.is_ascii_alphanumeric() || matches!(b, b'.' | b'_' | b'-'))
    {
        return Err("may contain only ASCII letters, digits, '.', '_' and '-'".to_string());
    }
    Ok(())
}

fn default_kid(key: &SigningKey) -> String {
    let hash = Sha256::digest(key.verifying_key().as_bytes());
    let mut hex = String::with_capacity(16);
    for byte in &hash[..8] {
        use std::fmt::Write;
        let _ = write!(&mut hex, "{byte:02x}");
    }
    hex
}

#[cfg(test)]
mod tests {
    use super::*;
    use ed25519_dalek::{Signature, Verifier, VerifyingKey};

    fn test_seed_b64() -> String {
        URL_SAFE_NO_PAD.encode([7u8; 32])
    }

    #[test]
    fn canonical_payload_sorts_plat5_headers_and_excludes_signature() {
        let payload = canonical_payload(
            "k-2026-11",
            1700000000,
            "post",
            "/projects/01jbx/plans?limit=10",
            "550e8400-e29b-41d4-a716-446655440000",
            [
                ("X-Plat5-Project-Id", "01jbx"),
                ("X-Plat5-Signature", "should-be-ignored"),
                ("X-Plat5-Organization-Id", "01jbw"),
                ("X-Plat5-Member-Id", "01jby"),
                ("X-Plat5-Principal", "person"),
                ("X-Other", "ignored"),
            ],
        );
        assert_eq!(
            payload,
            "v1\n\
             k-2026-11\n\
             1700000000\n\
             POST\n\
             /projects/01jbx/plans?limit=10\n\
             550e8400-e29b-41d4-a716-446655440000\n\
             x-plat5-member-id:01jby\n\
             x-plat5-organization-id:01jbw\n\
             x-plat5-principal:person\n\
             x-plat5-project-id:01jbx\n"
        );
    }

    #[test]
    fn canonical_payload_supports_public_routes_with_no_plat5_headers() {
        let payload = canonical_payload(
            "k1",
            1700000000,
            "GET",
            "/health",
            "req-1",
            std::iter::empty(),
        );
        assert_eq!(payload, "v1\nk1\n1700000000\nGET\n/health\nreq-1\n");
    }

    #[test]
    fn signs_and_verifies_ed25519_round_trip() {
        let signer = UpstreamSigner::from_seed_b64url(&test_seed_b64(), Some("k-1")).unwrap();
        let header = signer.sign(
            "GET",
            "/projects/01jbx/items?active=true",
            "req-123",
            [
                ("X-Plat5-Project-Id", "01jbx"),
                ("X-Plat5-Member-Id", "01jby"),
            ],
            1700000042,
        );
        let parts: Vec<&str> = header.split(',').collect();
        assert_eq!(parts.len(), 4);
        assert_eq!(parts[0], "v1");
        assert_eq!(parts[1], "kid=k-1");
        assert_eq!(parts[2], "t=1700000042");
        let sig_b64 = parts[3].strip_prefix("sig=").unwrap();
        assert_eq!(sig_b64.len(), 86);
        let sig_bytes: [u8; 64] = URL_SAFE_NO_PAD.decode(sig_b64).unwrap().try_into().unwrap();
        let sig = Signature::from_bytes(&sig_bytes);

        let verifying_key: VerifyingKey = SigningKey::from_bytes(&[7u8; 32]).verifying_key();
        let expected_payload = canonical_payload(
            "k-1",
            1700000042,
            "GET",
            "/projects/01jbx/items?active=true",
            "req-123",
            [
                ("X-Plat5-Member-Id", "01jby"),
                ("X-Plat5-Project-Id", "01jbx"),
            ],
        );
        verifying_key
            .verify(expected_payload.as_bytes(), &sig)
            .expect("signature must verify");
    }

    #[test]
    fn default_kid_is_16_hex_chars_from_public_key_sha256() {
        let signer = UpstreamSigner::from_seed_b64url(&test_seed_b64(), None).unwrap();
        assert_eq!(signer.kid().len(), 16);
        assert!(signer.kid().bytes().all(|b| matches!(b, b'0'..=b'9' | b'a'..=b'f')));
        // Deterministic for the same seed.
        let signer2 = UpstreamSigner::from_seed_b64url(&test_seed_b64(), None).unwrap();
        assert_eq!(signer.kid(), signer2.kid());
    }

    #[test]
    fn rejects_invalid_seed_and_kid() {
        assert!(UpstreamSigner::from_seed_b64url("not-base64!!!", None).is_err());
        assert!(UpstreamSigner::from_seed_b64url(&URL_SAFE_NO_PAD.encode([1u8; 16]), None).is_err());
        assert!(UpstreamSigner::from_seed_b64url(&test_seed_b64(), Some("")).is_err());
        assert!(UpstreamSigner::from_seed_b64url(&test_seed_b64(), Some("bad kid")).is_err());
        assert!(UpstreamSigner::from_seed_b64url(&test_seed_b64(), Some("a,b")).is_err());
        assert!(UpstreamSigner::from_seed_b64url(&test_seed_b64(), Some(&"a".repeat(65))).is_err());
    }
}
