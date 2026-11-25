use serde_json::{json, Value};

use super::*;

fn base() -> Value {
    json!({
        "kinds": {
            "organization": { "path": "/orgs/{organization}", "list_permission": "organization:read" },
            "project": { "parent": "organization", "path": "/projects/{project}", "list_permission": "project:read" }
        },
        "roles": {
            "admin": { "permissions": ["*"], "bindable_at": ["organization", "project"] },
            "member": { "permissions": ["organization:read"], "bindable_at": ["organization"] }
        },
        "creator_role": "admin",
        "defaults": { "rate_limit": ["standard"] },
        "rate_limits": {
            "standard": { "requests": 60, "window": "60s" },
            "per-project": { "requests": 100, "window": "1s", "count_at": "project" },
            "per-org": { "requests": 300, "window": "1s", "count_at": "organization" }
        }
    })
}

fn run(v: Value) -> Result<Checked, Vec<FieldError>> {
    crate::schema::check_config(&v).unwrap_or_else(|e| panic!("schema: {e:?}"));
    let config: Config = serde_json::from_value(v).unwrap();
    check(&config)
}

fn errors_at(v: Value, path: &str) -> Vec<FieldError> {
    match run(v) {
        Err(errs) => {
            assert!(errs.iter().any(|e| e.path == path), "want {path}: {errs:?}");
            errs
        }
        Ok(_) => panic!("want invalid at {path}, got ok"),
    }
}

/// The base with one e10s scope holding `routes`.
fn with_routes(scope: &str, routes: Value) -> Value {
    let mut c = base();
    c["services"] = json!({ "e10s": {
        "url": "http://e10s:3000",
        "scopes": { scope: { "routes": routes } }
    } });
    c
}

fn route(scope: &str, route: Value) -> Value {
    with_routes(scope, json!([route]))
}

const AT: &str = "services.e10s.scopes";

#[test]
fn the_base_passes() {
    run(base()).unwrap();
    run(route(
        "project",
        json!({ "path": "/", "methods": ["GET"], "permissions": ["organization:read"] }),
    ))
    .unwrap();
}

#[test]
fn kinds() {
    let mut c = base();
    c["kinds"]["project"]["parent"] = json!("folder");
    errors_at(c, "kinds.project.parent");
    let mut c = base();
    c["kinds"]["organization"]["parent"] = json!("project");
    errors_at(c, "kinds.organization.parent");
    let mut c = base();
    c["kinds"]["project"]["path"] = json!("/projects/{id}");
    errors_at(c, "kinds.project.path");

    // host template on root kind passes; child kind, wrong param, and duplicate suffix fail.
    let mut c = base();
    c["kinds"]["organization"]["host"] = json!("{organization}.api.example.com");
    run(c).unwrap();

    let mut c = base();
    c["kinds"]["project"]["host"] = json!("{project}.api.example.com");
    errors_at(c, "kinds.project.host");

    let mut c = base();
    c["kinds"]["organization"]["host"] = json!("{org}.api.example.com");
    errors_at(c, "kinds.organization.host");

    let mut c = base();
    c["kinds"]["organization"]["host"] = json!("{organization}.api.example.com");
    c["kinds"]["workspace"] = json!({
        "path": "/workspaces/{workspace}",
        "host": "{workspace}.api.example.com"
    });
    c["roles"]["admin"]["bindable_at"] = json!(["organization", "project", "workspace"]);
    errors_at(c, "kinds.workspace.host");

    // Non-tree kind errors (host) do not suppress downstream rule errors.
    let mut c = base();
    c["kinds"]["project"]["host"] = json!("{project}.api.example.com");
    c["creator_role"] = json!("ghost");
    let errs = run(c).unwrap_err();
    assert!(errs.iter().any(|e| e.path == "kinds.project.host"));
    assert!(errs.iter().any(|e| e.path == "creator_role"));

    // Sibling kinds cannot share the same collection path.
    let mut c = base();
    c["kinds"]["workspace"] = json!({ "path": "/orgs/{workspace}" });
    c["roles"]["admin"]["bindable_at"] = json!(["organization", "project", "workspace"]);
    errors_at(c, "kinds.workspace.path");

    // Duration overflow (exceeding 36500d) is refused.
    let mut c = base();
    c["deletion_grace"] = json!("200000d");
    errors_at(c, "deletion_grace");

    // rate_limits tiers pass schema and rules.
    let mut c = base();
    c["rate_limits"]["per-org"]["tiers"] = json!({
        "pro": { "requests": 1000, "window": "1s" },
        "enterprise": { "requests": 5000, "window": "1s" }
    });
    run(c).unwrap();

    // custom_roles and permissions catalog.
    let mut c = base();
    c["custom_roles"] = json!(true);
    c["permissions"] = json!({
        "reports:export": { "description": "Export reports", "bindable_at": ["project"] },
        "internal:admin": { "custom_role_assignable": false }
    });
    run(c).unwrap();

    let mut c = base();
    c["permissions"] = json!({
        "reports:export": { "bindable_at": ["ghost"] }
    });
    errors_at(c, "permissions.reports:export.bindable_at[0]");
}

#[test]
fn authorizer_all_or_nothing() {
    let mut c = base();
    c.as_object_mut().unwrap().remove("roles");
    c.as_object_mut().unwrap().remove("creator_role");
    c["authorizer"] = json!({
        "url": "http://topaz:8383",
        "timeout": "3s",
        "cache_ttl": "5s",
        "negative_cache_ttl": "1s",
        "token_env": "TOPAZ_API_KEY"
    });
    c["services"] = json!({
        "docs": {
            "url": "http://docs:3000",
            "scopes": {
                "project": {
                    "upstream_prefix": "/projects/{project.id}",
                    "routes": [
                        {
                            "path": "/docs",
                            "methods": ["GET"],
                            "permissions": "project:read"
                        },
                        {
                            "path": "/docs/{doc_id}",
                            "resource": { "type": "document", "id": "{project.id}:{doc_id}" },
                            "methods": {
                                "GET": { "permissions": "docs:read" },
                                "DELETE": {
                                    "permissions": ["docs:delete"],
                                    "resource": { "type": "document", "id": "{doc_id}" }
                                }
                            }
                        },
                        {
                            "path": "/membership",
                            "methods": ["GET"],
                            "permissions": "any_member"
                        }
                    ]
                },
                "public": {
                    "routes": [{ "path": "/health", "methods": ["GET"] }]
                }
            }
        }
    });
    let checked = run(c.clone()).unwrap();
    let routes = &checked.services["docs"].routes;
    let get_doc = routes
        .iter()
        .find(|r| r.method == "GET" && r.path.ends_with("/docs/{doc_id}"))
        .unwrap();
    assert_eq!(
        get_doc.permissions,
        Some(crate::config::Required::Permissions(vec!["docs:read".into()]))
    );
    assert_eq!(get_doc.resource.as_ref().unwrap().id, "{project.id}:{doc_id}");
    let del_doc = routes
        .iter()
        .find(|r| r.method == "DELETE" && r.path.ends_with("/docs/{doc_id}"))
        .unwrap();
    assert_eq!(del_doc.resource.as_ref().unwrap().id, "{doc_id}");

    // Mixing authorizer with roles / creator_role / custom_roles / permissions is refused.
    let mut bad = c.clone();
    bad["roles"] = json!({ "admin": { "permissions": ["*"], "bindable_at": ["organization"] } });
    errors_at(bad, "roles");

    let mut bad = c.clone();
    bad["creator_role"] = json!("admin");
    errors_at(bad, "creator_role");

    let mut bad = c.clone();
    bad["custom_roles"] = json!(true);
    errors_at(bad, "custom_roles");

    let mut bad = c.clone();
    bad["permissions"] = json!({ "docs:read": { "description": "Read" } });
    errors_at(bad, "permissions");

    // Neither roles nor authorizer is refused.
    let mut bad = base();
    bad.as_object_mut().unwrap().remove("roles");
    bad.as_object_mut().unwrap().remove("creator_role");
    let errs = run(bad).unwrap_err();
    assert!(errs.iter().any(|e| e.path == "roles"));
    assert!(errs.iter().any(|e| e.path == "creator_role"));

    // resource without top-level authorizer is refused.
    let bad = route(
        "project",
        json!({
            "path": "/docs/{doc_id}",
            "methods": ["GET"],
            "permissions": "docs:read",
            "resource": { "type": "document", "id": "{doc_id}" }
        }),
    );
    errors_at(bad, &format!("{AT}.project.routes[0].resource"));

    // resource on public/user scope is refused.
    let mut bad = c.clone();
    bad["services"]["docs"]["scopes"]["public"]["routes"][0]["resource"] =
        json!({ "type": "doc", "id": "1" });
    errors_at(bad, "services.docs.scopes.public.routes[0].resource");

    // resource on any_member route is refused.
    let mut bad = c.clone();
    bad["services"]["docs"]["scopes"]["project"]["routes"][2]["resource"] =
        json!({ "type": "project", "id": "{project.id}" });
    errors_at(bad, "services.docs.scopes.project.routes[2].resource");

    // Unknown placeholder in resource.id is refused.
    let mut bad = c.clone();
    bad["services"]["docs"]["scopes"]["project"]["routes"][1]["methods"]["DELETE"]["resource"]
        ["id"] = json!("{nope}");
    errors_at(
        bad,
        "services.docs.scopes.project.routes[1].methods.DELETE.resource.id",
    );
}

#[test]
fn roles() {
    let mut c = base();
    c["roles"]["member"]["bindable_at"] = json!(["team"]);
    errors_at(c, "roles.member.bindable_at[0]");
    let mut c = base();
    c["roles"]["member"]["includes"] = json!(["ghost"]);
    errors_at(c, "roles.member.includes[0]");
    let mut c = base();
    c["roles"]["a"] = json!({ "includes": ["b"], "bindable_at": ["organization"] });
    c["roles"]["b"] = json!({ "includes": ["a"], "bindable_at": ["organization"] });
    errors_at(c, "roles.a.includes");
    let mut c = base();
    c["creator_role"] = json!("member");
    errors_at(c, "creator_role");
    let mut c = base();
    c["creator_role"] = json!("owner");
    errors_at(c, "creator_role");
}

// A role may grant a permission no route requires: one checked only
// through identity's evaluation API, or a typo, which grants nothing.
#[test]
fn a_role_may_grant_any_permission() {
    let mut c = base();
    c["roles"]["member"]["permissions"] = json!(["organization:read", "reports:export"]);
    run(c).unwrap();
    // And a route may require one no role grants.
    run(route(
        "project",
        json!({ "path": "/a", "methods": ["GET"], "permissions": ["nobody:has"] }),
    ))
    .unwrap();
}

#[test]
fn paths() {
    let bad = [
        "/a/",
        "/a//b",
        "/A",
        "/files/{name}.json",
        "/x{a}",
        "/{a}/{a}",
        "/{9a}",
    ];
    for p in bad {
        errors_at(
            route("public", json!({ "path": p, "methods": ["GET"] })),
            &format!("{AT}.public.routes[0].path"),
        );
    }
    // A route's own params can't be kind names.
    errors_at(
        route(
            "public",
            json!({ "path": "/x/{project}", "methods": ["GET"] }),
        ),
        &format!("{AT}.public.routes[0].path"),
    );
}

#[test]
fn a_child_collection() {
    let at = format!("{AT}.organization.routes[0].path");
    let r = |path: &str| {
        route(
            "organization",
            json!({ "path": path, "methods": ["GET"], "permissions": ["organization:read"] }),
        )
    };
    errors_at(r("/projects/archived"), &at);
    errors_at(r("/projects/{x}/y"), &at);
    // The collection itself, and a literal that only starts the same.
    run(r("/archived-projects")).unwrap();
    let mut c = r("/projects");
    c["services"]["e10s"]["scopes"]["organization"]["routes"][0]["methods"] = json!(["PUT"]);
    run(c).unwrap();
}

#[test]
fn scopes_and_permissions() {
    let c = route("team", json!({ "path": "/a", "methods": ["GET"] }));
    errors_at(c, &format!("{AT}.team"));
    errors_at(
        route("project", json!({ "path": "/a", "methods": ["GET"] })),
        &format!("{AT}.project.routes[0].permissions"),
    );
    errors_at(
        route(
            "project",
            json!({ "path": "/a", "methods": { "GET": null, "POST": { "permissions": ["organization:read"] } } }),
        ),
        &format!("{AT}.project.routes[0].methods.GET.permissions"),
    );
    run(route(
        "project",
        json!({ "path": "/a", "permissions": "any_member", "methods": { "GET": null, "POST": { "permissions": ["organization:read"] } } }),
    ))
    .unwrap();
    errors_at(
        route(
            "user",
            json!({ "path": "/user/a", "methods": ["GET"], "permissions": ["organization:read"] }),
        ),
        &format!("{AT}.user.routes[0].permissions"),
    );
    errors_at(
        route(
            "public",
            json!({ "path": "/a", "methods": { "POST": { "audit": true } } }),
        ),
        &format!("{AT}.public.routes[0].methods.POST.audit"),
    );
    errors_at(
        route(
            "user",
            json!({ "path": "/user/a", "methods": ["GET"], "allow_deleted": true }),
        ),
        &format!("{AT}.user.routes[0].allow_deleted"),
    );
}

#[test]
fn placeholders() {
    let ok = |scope: &str, prefix: Option<&str>, path: &str, upstream: Option<&str>| {
        let mut r = json!({ "path": path, "methods": ["GET"] });
        if scope != "user" && scope != "public" {
            r["permissions"] = json!("any_member");
        }
        if let Some(u) = upstream {
            r["upstream"] = json!(u);
        }
        let mut c = route(scope, r);
        if let Some(p) = prefix {
            c["services"]["e10s"]["scopes"][scope]["upstream_prefix"] = json!(p);
        }
        c
    };
    run(ok(
        "project",
        Some("/p/{project.id}/o/{organization.id}/m/{member.id}"),
        "/a/{x}",
        None,
    ))
    .unwrap();
    run(ok("project", None, "/a/{x}", Some("/b/{x}/{project.id}"))).unwrap();
    run(ok("user", Some("/users/{user.id}"), "/user/a", None)).unwrap();
    let at = |scope: &str, f: &str| format!("{AT}.{scope}.{f}");
    for (scope, prefix, upstream, field) in [
        (
            "organization",
            Some("/p/{project.id}"),
            None,
            "upstream_prefix",
        ),
        (
            "project",
            Some("/p/{project.slug}"),
            None,
            "upstream_prefix",
        ),
        ("project", Some("/p/{x}"), None, "upstream_prefix"),
        ("project", Some("/p/{user.id}"), None, "upstream_prefix"),
        ("project", None, Some("/p/{y}"), "routes[0].upstream"),
        ("project", None, Some("/p/{team.id}"), "routes[0].upstream"),
        ("user", None, Some("/p/{member.id}"), "routes[0].upstream"),
        ("public", None, Some("/p/{user.id}"), "routes[0].upstream"),
        ("public", None, Some("/p/{x"), "routes[0].upstream"),
    ] {
        let path = if scope == "user" {
            "/user/a/{x}"
        } else {
            "/a/{x}"
        };
        errors_at(ok(scope, prefix, path, upstream), &at(scope, field));
    }
}

#[test]
fn rate_limits() {
    let r = |scope: &str, rl: Value| {
        let mut r = json!({ "path": "/a", "methods": ["GET"], "rate_limit": rl });
        if scope == "organization" || scope == "project" {
            r["permissions"] = json!("any_member");
        }
        route(scope, r)
    };
    run(r("project", json!(["per-project", "per-org"]))).unwrap();
    let at = |scope: &str| format!("{AT}.{scope}.routes[0].rate_limit");
    errors_at(r("project", json!(["ghost"])), &at("project"));
    errors_at(
        r("organization", json!(["per-project"])),
        &at("organization"),
    );
    errors_at(r("public", json!(["per-org"])), &at("public"));
    let mut c = base();
    c["rate_limits"]["per-team"] = json!({ "requests": 1, "window": "1s", "count_at": "team" });
    errors_at(c, "rate_limits.per-team.count_at");
    // defaults.rate_limit names policies, none with count_at.
    let mut c = base();
    c["defaults"]["rate_limit"] = json!(["standard", "ghost"]);
    errors_at(c, "defaults.rate_limit[1]");
    let mut c = base();
    c["defaults"]["rate_limit"] = json!(["per-org"]);
    errors_at(c, "defaults.rate_limit[0]");
    // No policy name is special: `default` is an ordinary one, and the
    // standard policy can count at a kind when no default names it.
    let mut c = base();
    c["rate_limits"]["default"] =
        json!({ "requests": 1, "window": "1s", "count_at": "organization" });
    run(c).unwrap();
    // Omitted is defaults.rate_limit, in every scope; `false` is none.
    let mut c = with_routes(
        "public",
        json!([
            { "path": "/a", "methods": ["GET"] },
            { "path": "/b", "methods": ["GET"], "rate_limit": false }
        ]),
    );
    c["rate_limits"]["burst"] = json!({ "requests": 5, "window": "1s" });
    c["defaults"]["rate_limit"] = json!(["standard", "burst"]);
    let checked = run(c).unwrap();
    let routes = &checked.services["e10s"].routes;
    assert_eq!(routes[0].rate_limit, vec!["standard", "burst"]);
    assert!(routes[1].rate_limit.is_empty());
    // Rate limits and defaults are optional: omitted routes are unlimited.
    let mut c = with_routes("public", json!([{ "path": "/a", "methods": ["GET"] }]));
    c.as_object_mut().unwrap().remove("defaults");
    c.as_object_mut().unwrap().remove("rate_limits");
    let checked = run(c).unwrap();
    assert!(checked.services["e10s"].routes[0].rate_limit.is_empty());
}

#[test]
fn same_shape_twice() {
    let c = with_routes(
        "public",
        json!([
            { "path": "/a/{x}", "methods": ["GET"] },
            { "path": "/a/{y}", "methods": ["GET", "POST"] }
        ]),
    );
    let errs = errors_at(c, &format!("{AT}.public.routes[1].path"));
    assert!(errs[0]
        .message
        .contains(&format!("{AT}.public.routes[0].path")));
    // Across services.
    let mut c = route(
        "organization",
        json!({ "path": "/", "methods": ["GET"], "permissions": ["organization:read"] }),
    );
    c["services"]["identity"] = identity();
    let errs = errors_at(c, "services.identity.scopes.organization.routes[0].path");
    assert!(
        errs[0]
            .message
            .contains(&format!("{AT}.organization.routes[0].path")),
        "{errs:?}"
    );
    // A literal and a param differ.
    run(with_routes(
        "public",
        json!([
            { "path": "/a/b", "methods": ["GET"] },
            { "path": "/a/{y}", "methods": ["GET"] }
        ]),
    ))
    .unwrap();
}

#[test]
fn a_route_matching_the_route_checks_path() {
    let mut c = base();
    c["services"] = json!({ "identity": identity() });
    run(c).unwrap();

    let at = format!("{AT}.public.routes[0].path");
    let c = route(
        "public",
        json!({ "path": "/access/routes", "methods": ["GET"] }),
    );
    errors_at(c, &at);

    // A param matches any segment.
    let c = route(
        "public",
        json!({ "path": "/access/{what}", "methods": ["POST"] }),
    );
    errors_at(c, &at);

    // Prefixes and longer paths don't match.
    let c = with_routes(
        "public",
        json!([
            { "path": "/access", "methods": ["GET"] },
            { "path": "/access/routes/{id}", "methods": ["GET"] }
        ]),
    );
    run(c).unwrap();
}

/// A piece of identity's service file, as `plat5 routes generate` writes it.
fn identity() -> Value {
    json!({
        "url": "http://identity:3000",
        "scopes": {
            "organization": {
                "upstream_prefix": "/nodes/{organization.id}",
                "routes": [
                    { "path": "/", "methods": {
                        "GET": { "permissions": ["organization:read"] },
                        "PATCH": { "permissions": ["organization:write"] }
                    } },
                    { "path": "/membership", "upstream": "/nodes/{organization.id}/memberships/{member.id}",
                      "methods": {
                        "GET": { "permissions": "any_member" },
                        "DELETE": { "permissions": "any_member" }
                    } }
                ]
            },
            "user": { "routes": [
                { "path": "/orgs", "upstream": "/roots/organization", "methods": ["POST"] },
                { "path": "/user", "upstream": "/users/{user.id}", "methods": ["GET", "PATCH", "DELETE"] }
            ] }
        }
    })
}

// Identity is a service like any other: its file is what's served.
#[test]
fn identity_routes_are_served_as_written() {
    let mut c = base();
    c["services"] = json!({ "identity": identity() });
    let checked = run(c).unwrap();
    let routes = &checked.services["identity"].routes;
    let leave = routes
        .iter()
        .find(|r| r.method == "DELETE" && r.path == "/orgs/{organization}/membership")
        .unwrap();
    assert_eq!(
        leave.upstream,
        "/nodes/{organization.id}/memberships/{member.id}"
    );
    assert_eq!(leave.rate_limit, vec!["standard"]);
    assert!(routes
        .iter()
        .all(|r| !(r.method == "POST" && r.path.ends_with("/members"))));
    // The old resource overrides are unknown fields.
    let mut c = base();
    c["services"] =
        json!({ "identity": { "url": "http://identity:3000", "resources": { "invites": false } } });
    assert!(crate::schema::check_config(&c).is_err());
    assert!(serde_json::from_value::<Config>(c).is_err());
}

// A kind's list_permission is <kind name>:read when it names none.
#[test]
fn list_permission_defaults_to_the_kinds_read() {
    let mut c = base();
    c["kinds"]["project"]
        .as_object_mut()
        .unwrap()
        .remove("list_permission");
    c["kinds"]["environment"] = json!({
        "parent": "project", "path": "/envs/{environment}", "list_permission": "project:read"
    });
    c["roles"]["admin"]["bindable_at"] = json!(["organization", "project", "environment"]);
    run(c.clone()).unwrap();
    let config: Config = serde_json::from_value(c.clone()).unwrap();
    assert_eq!(config.kinds["project"].list_permission, "project:read");
    assert_eq!(config.kinds["environment"].list_permission, "project:read");
    // permission_name is gone: an unknown field.
    c["kinds"]["environment"]["permission_name"] = json!("project");
    assert!(crate::schema::check_config(&c).is_err());
    assert!(serde_json::from_value::<Config>(c).is_err());
}

// bind_creator is on unless a kind turns it off, and is sent only when off.
#[test]
fn bind_creator_defaults_on() {
    let mut c = base();
    c["kinds"]["project"]["bind_creator"] = json!(false);
    run(c.clone()).unwrap();
    let config: Config = serde_json::from_value(c).unwrap();
    assert!(config.kinds["organization"].bind_creator);
    assert!(!config.kinds["project"].bind_creator);
    let sent = serde_json::to_value(&config.kinds).unwrap();
    assert_eq!(sent["organization"].get("bind_creator"), None);
    assert_eq!(sent["project"]["bind_creator"], json!(false));
}
