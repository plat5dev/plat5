use crate::route_map::Required;

/// The permission that grants every permission. A role's `["*"]` reaches the gateway as is.
pub const EVERY_PERMISSION: &str = "*";

/// Whether the caller's permissions and membership satisfy a route's
/// `required_permissions` (docs/reference/config/services.md#required_permissions).
///
/// - `any_member` → requires actual membership in the tree (`is_member == true`).
/// - The caller holds `*` → allow.
/// - Otherwise they must share one: `[a, b]` means a or b.
pub fn permitted(required: &Required, granted: &[String], is_member: bool) -> bool {
    match required {
        Required::Nothing => true,
        Required::AnyMember => is_member,
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
        assert!(permitted(&any_of(&["read"]), &s(&["*"]), true));
    }

    #[test]
    fn any_one_permission_admits() {
        assert!(permitted(
            &any_of(&["read", "write"]),
            &s(&["write", "reports.export"]),
            true
        ));
        assert!(!permitted(&any_of(&["read"]), &s(&["write"]), true));
    }

    #[test]
    fn permissions_are_opaque() {
        assert!(!permitted(
            &any_of(&["project:read"]),
            &s(&["project:read:all", "project"]),
            true
        ));
    }

    #[test]
    fn a_role_with_nothing_passes_only_any_member_when_member() {
        assert!(!permitted(&any_of(&["read"]), &[], true));
        assert!(permitted(&Required::AnyMember, &[], true));
        assert!(!permitted(&Required::AnyMember, &[], false));
        assert!(!permitted(&Required::AnyMember, &s(&["*"]), false));
    }
}
