use crate::route_map::Required;

/// The permission that grants every permission. A role's `["*"]` reaches the gateway as is.
pub const EVERY_PERMISSION: &str = "*";

/// Whether the caller's permissions on the node satisfy a route's
/// `required_permissions` (docs/reference/config/services.md#required_permissions).
///
/// - `any_member` → any member of the tree.
/// - The caller holds `*` → allow.
/// - Otherwise they must share one: `[a, b]` means a or b.
pub fn permitted(required: &Required, granted: &[String]) -> bool {
    match required {
        Required::Nothing | Required::AnyMember => true,
        Required::AnyOf(_) if granted.iter().any(|have| have == EVERY_PERMISSION) => true,
        Required::AnyOf(list) => list
            .iter()
            .any(|need| granted.iter().any(|have| have == need)),
    }
}

#[cfg(test)]
mod tests {
    use super::*;

    fn s(permissions: &[&str]) -> Vec<String> {
        permissions.iter().map(|p| (*p).to_string()).collect()
    }

    fn any_of(permissions: &[&str]) -> Required {
        Required::AnyOf(s(permissions))
    }

    #[test]
    fn every_permission_passes() {
        assert!(permitted(&any_of(&["read"]), &s(&["*"])));
    }

    #[test]
    fn any_one_permission_admits() {
        assert!(permitted(
            &any_of(&["read", "write"]),
            &s(&["write", "reports.export"])
        ));
        assert!(!permitted(&any_of(&["read"]), &s(&["write"])));
    }

    #[test]
    fn permissions_are_opaque() {
        assert!(!permitted(
            &any_of(&["project:read"]),
            &s(&["project:read:all", "project"])
        ));
    }

    #[test]
    fn a_role_with_nothing_passes_only_any_member() {
        assert!(!permitted(&any_of(&["read"]), &[]));
        assert!(permitted(&Required::AnyMember, &[]));
    }
}
