/// Whether the caller's labels satisfy a route's `required_labels`.
///
/// - No `required_labels` on the route → allow.
/// - `granted == None` (a user, or a member whose role is unrestricted) → skip / allow.
/// - Otherwise require a nonempty intersection.
pub fn satisfies_required_labels(required: Option<&[String]>, granted: Option<&[String]>) -> bool {
    let Some(required) = required.filter(|r| !r.is_empty()) else {
        return true;
    };
    let Some(granted) = granted else {
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
    fn unrestricted_role_passes_required_labels() {
        let required = s(&["read"]);
        assert!(satisfies_required_labels(Some(required.as_slice()), None));
    }

    #[test]
    fn user_skips_required_labels() {
        let required = s(&["read", "write"]);
        assert!(satisfies_required_labels(Some(required.as_slice()), None));
    }

    #[test]
    fn role_label_hit() {
        let required = s(&["read", "write"]);
        let granted = s(&["write", "reports.export"]);
        assert!(satisfies_required_labels(
            Some(required.as_slice()),
            Some(granted.as_slice())
        ));
    }

    #[test]
    fn role_label_miss() {
        let required = s(&["read"]);
        let granted = s(&["write"]);
        assert!(!satisfies_required_labels(
            Some(required.as_slice()),
            Some(granted.as_slice())
        ));
    }

    #[test]
    fn empty_role_grants_nothing() {
        let required = s(&["read"]);
        let granted: Vec<String> = vec![];
        assert!(!satisfies_required_labels(
            Some(required.as_slice()),
            Some(granted.as_slice())
        ));
    }

    #[test]
    fn no_required_labels_allows_anything() {
        let granted = s(&["read"]);
        assert!(satisfies_required_labels(None, Some(granted.as_slice())));
        assert!(satisfies_required_labels(None, None));
        let empty: Vec<String> = vec![];
        assert!(satisfies_required_labels(
            Some(empty.as_slice()),
            Some(granted.as_slice())
        ));
    }
}
