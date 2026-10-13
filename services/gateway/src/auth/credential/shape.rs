//! The shape of an `X-API-Key` (docs/gateway-contract.md#credentials). The gateway
//! checks it before any identity call, so junk never costs a round trip. It does not
//! know the brand: identity decides whether a well-shaped value is one of its own.

const MAX_BRAND_LEN: usize = 32;
const SECRET_LEN: usize = 43;

/// What a credential's prefix says: a key is revoked, a session expires.
#[derive(Clone, Copy, Debug, PartialEq, Eq)]
pub enum Kind {
    Key,
    Session,
}

/// A well-shaped credential. `wire_prefix_len` is the length of
/// `{brand}-{kind}-{version}-`, the part before the secret.
#[derive(Clone, Copy, Debug, PartialEq, Eq)]
pub struct Shape {
    pub kind: Kind,
    pub wire_prefix_len: usize,
}

/// `{brand}-{k|s}-2-{secret}`, or a version 1 `{brand}-{sk|mk|ms}-1-{secret}`. The
/// brand is `[a-z][a-z0-9]*`, at most 32; the secret is 43 base64url characters.
pub fn parse(value: &str) -> Option<Shape> {
    let mut parts = value.splitn(4, '-');
    let brand = parts.next()?;
    let kind = parts.next()?;
    let version = parts.next()?;
    let secret = parts.next()?;
    if !valid_brand(brand) || !valid_secret(secret) {
        return None;
    }
    let kind = match (version, kind) {
        ("2", "k") | ("1", "sk") | ("1", "mk") => Kind::Key,
        ("2", "s") | ("1", "ms") => Kind::Session,
        _ => return None,
    };
    Some(Shape {
        kind,
        wire_prefix_len: value.len() - secret.len(),
    })
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

#[cfg(test)]
mod tests {
    use super::*;

    fn secret() -> String {
        "A".repeat(SECRET_LEN)
    }

    #[test]
    fn version_2_keys_and_sessions() {
        let key = format!("plat5-k-2-{}", secret());
        assert_eq!(
            parse(&key),
            Some(Shape {
                kind: Kind::Key,
                wire_prefix_len: "plat5-k-2-".len()
            })
        );
        let session = format!("acme1-s-2-{}", secret());
        assert_eq!(parse(&session).map(|s| s.kind), Some(Kind::Session));
    }

    #[test]
    fn version_1_still_parses() {
        for (letters, kind) in [("sk", Kind::Key), ("mk", Kind::Key), ("ms", Kind::Session)] {
            let v = format!("plat5-{letters}-1-{}", secret());
            assert_eq!(parse(&v).map(|s| s.kind), Some(kind), "{letters}");
        }
    }

    #[test]
    fn the_secret_may_contain_dashes() {
        let v = format!("plat5-k-2-{}-{}", "a".repeat(20), "b".repeat(22));
        assert_eq!(
            parse(&v).map(|s| s.wire_prefix_len),
            Some("plat5-k-2-".len())
        );
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
            format!("plat5-k-1-{s}"),
            format!("plat5-sk-2-{s}"),
            format!("plat5-k-3-{s}"),
            format!("plat5-x-2-{s}"),
            format!("plat5-k-2-{}", &s[1..]),
            format!("plat5-k-2-{s}A"),
            format!("plat5-k-2-{}=", &s[1..]),
            "sk_live_abcdefgh".to_string(),
        ] {
            assert_eq!(parse(&v), None, "{v}");
        }
    }
}
