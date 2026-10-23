//! The shape of an `X-API-Key` (docs/concepts/access.md#credentials). The gateway
//! checks it before any identity call, so junk never costs a round trip. It does not
//! know the brand: identity decides whether a well-shaped value is one of its own.

const MAX_BRAND_LEN: usize = 32;
const SECRET_LEN: usize = 43;

/// `{brand}-k-2-{secret}`: a brand of `[a-z][a-z0-9]*`, at most 32, and a
/// 43-character base64url secret. Some(length of the wire prefix, `{brand}-k-2-`).
pub fn key_wire_prefix_len(value: &str) -> Option<usize> {
    let mut parts = value.splitn(4, '-');
    let brand = parts.next()?;
    let kind = parts.next()?;
    let version = parts.next()?;
    let secret = parts.next()?;
    if kind != "k" || version != "2" || !valid_brand(brand) || !valid_secret(secret) {
        return None;
    }
    Some(value.len() - secret.len())
}

fn valid_brand(brand: &str) -> bool {
    let bytes = brand.as_bytes();
    !bytes.is_empty()
        && bytes.len() <= MAX_BRAND_LEN
        && bytes[0].is_ascii_lowercase()
        && bytes
            .iter()
            .all(|b| b.is_ascii_lowercase() || b.is_ascii_digit())
}

fn valid_secret(secret: &str) -> bool {
    secret.len() == SECRET_LEN
        && secret
            .bytes()
            .all(|b| b.is_ascii_alphanumeric() || b == b'-' || b == b'_')
}

/// Characters of the secret kept after the wire prefix. Same as identity's
/// `apikey.PrefixDisplayLen`.
pub const KEY_DISPLAY_LEN: usize = 4;

/// Identity's display prefix for a well-shaped key: the wire prefix plus
/// [`KEY_DISPLAY_LEN`] characters. Never the whole key.
pub fn key_display_prefix(key: &str) -> Option<String> {
    let n = key_wire_prefix_len(key)? + KEY_DISPLAY_LEN;
    key.get(..n).map(str::to_string)
}

#[cfg(test)]
mod tests {
    use super::*;

    fn secret() -> String {
        "A".repeat(SECRET_LEN)
    }

    #[test]
    fn version_2_keys() {
        let key = format!("plat5-k-2-{}", secret());
        assert_eq!(key_wire_prefix_len(&key), Some("plat5-k-2-".len()));
        assert_eq!(key_display_prefix(&key).as_deref(), Some("plat5-k-2-AAAA"));
    }

    #[test]
    fn the_secret_may_contain_dashes() {
        let v = format!("plat5-k-2-{}-{}", "a".repeat(20), "b".repeat(22));
        assert_eq!(key_wire_prefix_len(&v), Some("plat5-k-2-".len()));
    }

    #[test]
    fn rejects_other_shapes() {
        let s = secret();
        for v in [
            String::new(),
            "nope".to_string(),
            format!("Plat5-k-2-{s}"),
            format!("1plat5-k-2-{s}"),
            format!("{}-k-2-{s}", "a".repeat(MAX_BRAND_LEN + 1)),
            // Other formats: sessions, other versions, other key types.
            format!("plat5-s-2-{s}"),
            format!("plat5-sk-1-{s}"),
            format!("plat5-mk-1-{s}"),
            format!("plat5-ms-1-{s}"),
            format!("plat5-k-1-{s}"),
            format!("plat5-k-3-{s}"),
            format!("plat5-k-2-{}", &s[1..]),
            format!("plat5-k-2-{s}A"),
            format!("plat5-k-2-{}=", &s[1..]),
            "sk_live_abcdefgh".to_string(),
        ] {
            assert_eq!(key_wire_prefix_len(&v), None, "{v}");
            assert_eq!(key_display_prefix(&v), None, "{v}");
        }
    }
}
