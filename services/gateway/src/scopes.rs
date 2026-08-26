/// Whether an API key's scope list satisfies a route's `required_scopes`.
///
/// - No `required_scopes` on the route → allow.
/// - `key_scopes == None` (JWT or unrestricted key) → skip / allow.
/// - Otherwise require a nonempty intersection.
pub fn key_satisfies_required_scopes(
    required: Option<&[String]>,
    key_scopes: Option<&[String]>,
) -> bool {
    let Some(required) = required.filter(|r| !r.is_empty()) else {
        return true;
    };
    let Some(granted) = key_scopes else {
        return true;
    };
    required
        .iter()
        .any(|need| granted.iter().any(|have| have == need))
}

#[cfg(test)]
mod tests {
    use super::*;

    fn s(labels: &[&str]) -> Vec<String> {
        labels.iter().map(|l| (*l).to_string()).collect()
    }

    #[test]
    fn unrestricted_key_passes_required_scopes() {
        let required = s(&["read"]);
        assert!(key_satisfies_required_scopes(
            Some(required.as_slice()),
            None
        ));
    }

    #[test]
    fn jwt_skips_required_scopes() {
        let required = s(&["read", "write"]);
        assert!(key_satisfies_required_scopes(
            Some(required.as_slice()),
            None
        ));
    }

    #[test]
    fn scoped_key_hit() {
        let required = s(&["read", "write"]);
        let granted = s(&["write", "reports.export"]);
        assert!(key_satisfies_required_scopes(
            Some(required.as_slice()),
            Some(granted.as_slice())
        ));
    }

    #[test]
    fn scoped_key_miss() {
        let required = s(&["read"]);
        let granted = s(&["write"]);
        assert!(!key_satisfies_required_scopes(
            Some(required.as_slice()),
            Some(granted.as_slice())
        ));
    }

    #[test]
    fn empty_key_scopes_grant_nothing() {
        let required = s(&["read"]);
        let granted: Vec<String> = vec![];
        assert!(!key_satisfies_required_scopes(
            Some(required.as_slice()),
            Some(granted.as_slice())
        ));
    }

    #[test]
    fn no_required_scopes_allows_anything() {
        let granted = s(&["read"]);
        assert!(key_satisfies_required_scopes(
            None,
            Some(granted.as_slice())
        ));
        assert!(key_satisfies_required_scopes(None, None));
        let empty: Vec<String> = vec![];
        assert!(key_satisfies_required_scopes(
            Some(empty.as_slice()),
            Some(granted.as_slice())
        ));
    }
}
