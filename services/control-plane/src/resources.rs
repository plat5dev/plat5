//! Identity's and audit's resources at their defaults
//! (docs/reference/config/resources.md). Plat5 fixes each one's operations and
//! upstreams; the deployment's overrides move or gate them. Permissions hold
//! `{kind}`, filled in with the kind each route is for.

use crate::config::{Config, Required, ANY_MEMBER, AUDIT, IDENTITY, USER_SCOPE};

/// One resource for one kind, or for the user scope.
#[derive(Clone, Debug)]
pub struct ResourceDefault {
    pub name: &'static str,
    /// The kind it's for: `kinds.<kind>.resources` overrides it. `None` for the
    /// user-scope resources, overridden only under `resources`.
    pub kind: Option<String>,
    /// Whether `resources` (for every kind) overrides it. `roots` is set per kind.
    pub shared: bool,
    pub scope: String,
    pub path: String,
    pub verbs: Vec<DefaultVerb>,
    pub allow_deleted: bool,
}

#[derive(Clone, Debug)]
pub struct DefaultVerb {
    pub method: &'static str,
    pub required: Option<Required>,
    pub upstream: String,
}

/// The collections whose `false` also removes this resource, outermost first
/// (docs/reference/config/resources.md): an item route has no use without its
/// collection.
pub fn collections_of(name: &str) -> &'static [&'static str] {
    match name {
        "service_account" | "keys" => &["service_accounts"],
        "key" => &["service_accounts", "keys"],
        "invite" => &["invites"],
        "binding" => &["bindings"],
        "member" => &["members"],
        "my_key" => &["my_keys"],
        _ => &[],
    }
}

fn perm(p: &str) -> Option<Required> {
    Some(Required::Permissions(vec![p.to_string()]))
}

fn any_member() -> Option<Required> {
    Some(ANY_MEMBER)
}

struct Builder<'a> {
    out: Vec<ResourceDefault>,
    kind: Option<&'a str>,
}

impl Builder<'_> {
    fn add(
        &mut self,
        name: &'static str,
        scope: &str,
        path: &str,
        verbs: Vec<(&'static str, Option<Required>, String)>,
    ) -> &mut ResourceDefault {
        self.out.push(ResourceDefault {
            name,
            kind: self.kind.map(str::to_string),
            shared: true,
            scope: scope.to_string(),
            path: path.to_string(),
            verbs: verbs
                .into_iter()
                .map(|(method, required, upstream)| DefaultVerb {
                    method,
                    required,
                    upstream,
                })
                .collect(),
            allow_deleted: false,
        });
        self.out.last_mut().expect("just pushed")
    }
}

/// Every default resource of the service named `service` (`identity` or
/// `audit`), for every kind in the config, then the user scope's.
pub fn defaults(config: &Config, service: &str) -> Vec<ResourceDefault> {
    let mut b = Builder {
        out: Vec::new(),
        kind: None,
    };
    for (k, kind) in &config.kinds {
        b.kind = Some(k);
        let node = format!("/nodes/{{{k}.id}}");
        let is_root = kind.is_root();
        if service == AUDIT {
            b.add(
                "events",
                k,
                "/audit-events",
                vec![("GET", perm("audit:read"), format!("{node}/audit-events"))],
            );
            continue;
        }
        let read = || perm("{kind}:bindings:read");
        let write = || perm("{kind}:bindings:write");
        b.add(
            "node",
            k,
            "/",
            vec![
                ("GET", perm("{kind}:read"), node.clone()),
                ("PATCH", perm("{kind}:write"), node.clone()),
                ("DELETE", perm("{kind}:delete"), node.clone()),
            ],
        );
        b.add(
            "restore",
            k,
            "/restore",
            vec![("POST", perm("{kind}:delete"), format!("{node}/restore"))],
        )
        .allow_deleted = true;
        if let Some(parent) = &kind.parent {
            let up = format!("/nodes/{{{parent}.id}}/children/{k}");
            b.add(
                "children",
                parent,
                kind.collection(),
                vec![
                    ("GET", any_member(), up.clone()),
                    ("POST", perm("{kind}:create"), up),
                ],
            );
        }
        let mut membership = vec![(
            "GET",
            any_member(),
            format!("{node}/memberships/{{member.id}}"),
        )];
        if is_root {
            membership.push((
                "DELETE",
                any_member(),
                format!("{node}/members/{{member.id}}"),
            ));
        }
        b.add("membership", k, "/membership", membership);
        let up = format!("{node}/bindings");
        b.add(
            "bindings",
            k,
            "/bindings",
            vec![("GET", read(), up.clone()), ("POST", write(), up)],
        );
        let up = format!("{node}/bindings/{{binding_id}}");
        b.add(
            "binding",
            k,
            "/bindings/{binding_id}",
            vec![("PATCH", write(), up.clone()), ("DELETE", write(), up)],
        );
        let up = format!("{node}/service-accounts");
        b.add(
            "service_accounts",
            k,
            "/service-accounts",
            vec![("GET", read(), up.clone()), ("POST", write(), up)],
        );
        let up = format!("{node}/service-accounts/{{service_account_id}}");
        b.add(
            "service_account",
            k,
            "/service-accounts/{service_account_id}",
            vec![
                ("GET", read(), up.clone()),
                ("PATCH", write(), up.clone()),
                ("DELETE", write(), up),
            ],
        );
        let up = format!("{node}/service-accounts/{{service_account_id}}/keys");
        b.add(
            "keys",
            k,
            "/service-accounts/{service_account_id}/keys",
            vec![("GET", write(), up.clone()), ("POST", write(), up)],
        );
        let up = format!("{node}/service-accounts/{{service_account_id}}/keys/{{key_id}}");
        b.add(
            "key",
            k,
            "/service-accounts/{service_account_id}/keys/{key_id}",
            vec![("DELETE", write(), up)],
        );
        let invite = || {
            if is_root {
                write()
            } else {
                perm("guests:write")
            }
        };
        let up = format!("{node}/invites");
        b.add(
            "invites",
            k,
            "/invites",
            vec![("GET", invite(), up.clone()), ("POST", invite(), up)],
        );
        b.add(
            "invite",
            k,
            "/invites/{invite_id}",
            vec![("DELETE", invite(), format!("{node}/invites/{{invite_id}}"))],
        );
        b.add(
            "roles",
            k,
            "/roles",
            vec![("GET", any_member(), format!("{node}/roles"))],
        );
        if is_root {
            let up = format!("{node}/members");
            b.add(
                "members",
                k,
                "/members",
                vec![("GET", read(), up.clone()), ("POST", write(), up)],
            );
            let up = format!("{node}/members/{{member_id}}");
            b.add(
                "member",
                k,
                "/members/{member_id}",
                vec![("GET", any_member(), up.clone()), ("DELETE", write(), up)],
            );
            b.add(
                "roots",
                USER_SCOPE,
                kind.collection(),
                vec![("POST", None, format!("/roots/{k}"))],
            )
            .shared = false;
        }
    }

    b.kind = None;
    let user = "/users/{user.id}";
    if service == AUDIT {
        b.add(
            "my_events",
            USER_SCOPE,
            "/user/audit-events",
            vec![("GET", None, format!("{user}/audit-events"))],
        );
        return b.out;
    }
    debug_assert_eq!(service, IDENTITY);
    b.add(
        "me",
        USER_SCOPE,
        "/user",
        vec![
            ("GET", None, user.to_string()),
            ("DELETE", None, user.to_string()),
        ],
    );
    b.add(
        "my_nodes",
        USER_SCOPE,
        "/user/nodes",
        vec![("GET", None, format!("{user}/nodes"))],
    );
    let up = format!("{user}/keys");
    b.add(
        "my_keys",
        USER_SCOPE,
        "/user/keys",
        vec![("GET", None, up.clone()), ("POST", None, up)],
    );
    b.add(
        "my_key",
        USER_SCOPE,
        "/user/keys/{key_id}",
        vec![("DELETE", None, format!("{user}/keys/{{key_id}}"))],
    );
    b.add(
        "redeem",
        USER_SCOPE,
        "/user/invites/redeem",
        vec![("POST", None, format!("{user}/invites/redeem"))],
    );
    b.out
}
