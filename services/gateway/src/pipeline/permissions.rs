/// The label that grants every label. A role's `["*"]` reaches the gateway as is.
pub const EVERY_LABEL: &str = "*";

/// Whether the caller's labels on the node satisfy a route's `required_labels`
/// (docs/reference/config/routes.md#required_labels).
///
/// - `[]` → any member of the tree.
/// - The caller holds `*` → allow.
/// - Otherwise they must share one label: `[a, b]` means a or b.
pub fn satisfies_required_labels(required: &[String], granted: &[String]) -> bool {
    if required.is_empty() || granted.iter().any(|have| have == EVERY_LABEL) {
        return true;
    }
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
    fn every_label_passes_required_labels() {
        assert!(satisfies_required_labels(&s(&["read"]), &s(&["*"])));
    }

    #[test]
    fn any_one_label_admits() {
        assert!(satisfies_required_labels(
            &s(&["read", "write"]),
            &s(&["write", "reports.export"])
        ));
        assert!(!satisfies_required_labels(&s(&["read"]), &s(&["write"])));
    }

    #[test]
    fn labels_are_opaque() {
        assert!(!satisfies_required_labels(
            &s(&["project:read"]),
            &s(&["project:read:all", "project"])
        ));
    }

    #[test]
    fn empty_role_grants_nothing_but_an_empty_route_admits_any_member() {
        assert!(!satisfies_required_labels(&s(&["read"]), &[]));
        assert!(satisfies_required_labels(&[], &[]));
    }
}
