//! The generated catalog: identity's edge routes and audit's read routes for a
//! set of kinds, as two services (docs/reference/api/edge-catalog.md). Written
//! in the routes-file form, for a team to keep and edit.

use std::collections::BTreeMap;

use serde_json::{json, Map, Value};

use crate::config::{Config, Kind};

/// Where the gateway reaches identity's and audit's public APIs in Plat5's
/// compose. A team edits it like any routes file.
pub const IDENTITY_URL: &str = "http://identity:3000";
pub const AUDIT_URL: &str = "http://audit:3002";

/// One route: path, scope, upstream, and each method with its labels. Methods
/// that share labels are written as a list, unless one is audited apart.
struct Gen {
    path: String,
    scope: String,
    upstream: String,
    methods: Vec<(&'static str, Option<Vec<String>>)>,
    resolve_deleted: bool,
    /// The read audited although reads aren't by default: one that returns
    /// live tokens.
    audited: Option<&'static str>,
}

impl Gen {
    fn to_value(&self) -> Value {
        let mut route = Map::new();
        route.insert("path".into(), json!(self.path));
        route.insert("scope".into(), json!(self.scope));
        route.insert("upstream".into(), json!(self.upstream));
        let first = &self.methods[0].1;
        if self.audited.is_none() && self.methods.iter().all(|(_, l)| l == first) {
            let verbs: Vec<&str> = self.methods.iter().map(|(m, _)| *m).collect();
            route.insert("methods".into(), json!(verbs));
            if let Some(labels) = first {
                route.insert("required_labels".into(), json!(labels));
            }
        } else {
            let mut map = Map::new();
            for (verb, labels) in &self.methods {
                let mut cfg = Map::new();
                if let Some(l) = labels {
                    cfg.insert("required_labels".into(), json!(l));
                }
                if self.audited == Some(*verb) {
                    cfg.insert("audit".into(), json!(true));
                }
                map.insert(
                    verb.to_string(),
                    if cfg.is_empty() {
                        Value::Null
                    } else {
                        Value::Object(cfg)
                    },
                );
            }
            route.insert("methods".into(), Value::Object(map));
        }
        if self.resolve_deleted {
            route.insert("resolve_deleted".into(), json!(true));
        }
        Value::Object(route)
    }
}

fn labels(names: &[&str]) -> Option<Vec<String>> {
    Some(names.iter().map(|s| s.to_string()).collect())
}

/// The two services for these kinds, keyed `identity` and `audit`. The kinds
/// must already hold rules 1 and 2.
pub fn generate(kinds: &BTreeMap<String, Kind>) -> Value {
    let config = Config::from_kinds(kinds.clone());
    let sign_in = config.sign_in_kind().unwrap_or("directory").to_string();
    let mut identity = Vec::new();
    let mut audit = Vec::new();

    for name in config.creatable_kinds() {
        for chain in config.chains(name) {
            node_routes(&config, &sign_in, &chain, &mut identity, &mut audit);
        }
    }

    let me = |path: &str, upstream: &str, methods: &[&'static str]| Gen {
        path: path.into(),
        scope: sign_in.clone(),
        upstream: upstream.into(),
        methods: methods.iter().map(|m| (*m, None)).collect(),
        resolve_deleted: false,
        audited: None,
    };
    identity.push(me(
        "/me",
        "/members/{subject.member_id}",
        &["GET", "DELETE"],
    ));
    identity.push(me(
        "/me/nodes",
        "/members/{subject.member_id}/nodes",
        &["GET"],
    ));
    identity.push(me(
        "/me/keys",
        "/members/{subject.member_id}/keys",
        &["GET", "POST"],
    ));
    identity.push(me(
        "/me/keys/{key_id}",
        "/members/{subject.member_id}/keys/{path.key_id}",
        &["DELETE"],
    ));
    identity.push(me(
        "/me/invites/redeem",
        "/members/{subject.member_id}/invites/redeem",
        &["POST"],
    ));
    audit.push(me(
        "/me/audit-events",
        "/members/{subject.member_id}/audit-events",
        &["GET"],
    ));

    let service = |url: &str, routes: Vec<Gen>| json!({ "url": url, "routes": routes.iter().map(Gen::to_value).collect::<Vec<_>>() });
    let mut out = Map::new();
    out.insert("identity".into(), service(IDENTITY_URL, identity));
    out.insert("audit".into(), service(AUDIT_URL, audit));
    Value::Object(out)
}

/// The routes for one kind reached through one chain of kinds, root first.
fn node_routes(
    config: &Config,
    sign_in: &str,
    chain: &[String],
    identity: &mut Vec<Gen>,
    audit: &mut Vec<Gen>,
) {
    let kind = chain.last().expect("a chain names its kind").as_str();
    let root = chain[0].as_str();
    let is_root = chain.len() == 1;
    let segment = |k: &str| config.kinds[k].segment(k);
    let path_of = |c: &[String]| -> String {
        c.iter()
            .map(|k| format!("/{}/{{{k}}}", segment(k)))
            .collect::<String>()
    };
    let node = path_of(chain);
    let id = format!("/nodes/{{subject.{kind}_id}}");
    let l = |suffix: &str| format!("{kind}:{suffix}");
    let route = |path: String,
                 scope: &str,
                 upstream: String,
                 methods: Vec<(&'static str, Option<Vec<String>>)>| Gen {
        path,
        scope: scope.to_string(),
        upstream,
        methods,
        resolve_deleted: false,
        audited: None,
    };

    // Creating, and listing under the parent.
    if is_root {
        identity.push(route(
            format!("/{}", segment(kind)),
            sign_in,
            format!("/roots/{kind}"),
            vec![("POST", None)],
        ));
    } else {
        let parent = chain[chain.len() - 2].as_str();
        identity.push(route(
            format!("{}/{}", path_of(&chain[..chain.len() - 1]), segment(kind)),
            parent,
            format!("/nodes/{{subject.{parent}_id}}/children/{kind}"),
            vec![("GET", labels(&[])), ("POST", labels(&[&l("create")]))],
        ));
    }

    identity.push(route(
        node.clone(),
        kind,
        id.clone(),
        vec![
            ("GET", labels(&[&l("read")])),
            ("PATCH", labels(&[&l("write")])),
            ("DELETE", labels(&[&l("delete")])),
        ],
    ));
    let mut restore = route(
        format!("{node}/restore"),
        kind,
        format!("{id}/restore"),
        vec![("POST", labels(&[&l("delete")]))],
    );
    restore.resolve_deleted = true;
    identity.push(restore);

    let bindings_read = l("bindings:read");
    let bindings_write = l("bindings:write");
    let sa_write = l("service-accounts:write");
    identity.push(route(
        format!("{node}/bindings"),
        kind,
        format!("{id}/bindings"),
        vec![
            ("GET", labels(&[&bindings_read])),
            ("POST", labels(&[&bindings_write])),
        ],
    ));
    identity.push(route(
        format!("{node}/bindings/{{binding_id}}"),
        kind,
        format!("{id}/bindings/{{path.binding_id}}"),
        vec![
            ("PATCH", labels(&[&bindings_write])),
            ("DELETE", labels(&[&bindings_write])),
        ],
    ));
    identity.push(route(
        format!("{node}/access"),
        kind,
        format!("{id}/access"),
        vec![("GET", labels(&[&bindings_read]))],
    ));
    identity.push(route(
        format!("{node}/service-accounts"),
        kind,
        format!("{id}/service-accounts"),
        vec![
            ("GET", labels(&[&bindings_read])),
            ("POST", labels(&[&sa_write])),
        ],
    ));
    identity.push(route(
        format!("{node}/service-accounts/{{service_account}}"),
        kind,
        format!("{id}/service-accounts/{{path.service_account}}"),
        vec![
            ("GET", labels(&[&bindings_read])),
            ("PATCH", labels(&[&sa_write])),
            ("DELETE", labels(&[&sa_write])),
        ],
    ));
    identity.push(route(
        format!("{node}/service-accounts/{{service_account}}/keys"),
        kind,
        format!("{id}/service-accounts/{{path.service_account}}/keys"),
        vec![
            ("GET", labels(&[&sa_write])),
            ("POST", labels(&[&sa_write])),
        ],
    ));
    identity.push(route(
        format!("{node}/service-accounts/{{service_account}}/keys/{{key_id}}"),
        kind,
        format!("{id}/service-accounts/{{path.service_account}}/keys/{{path.key_id}}"),
        vec![("DELETE", labels(&[&sa_write]))],
    ));

    // Invites: a root's own admit members; below a root they can admit guests.
    let invites = if is_root {
        bindings_write.clone()
    } else {
        format!("{root}:guests:write")
    };
    let mut list = route(
        format!("{node}/invites"),
        kind,
        format!("{id}/invites"),
        vec![("GET", labels(&[&invites])), ("POST", labels(&[&invites]))],
    );
    list.audited = Some("GET");
    identity.push(list);
    identity.push(route(
        format!("{node}/invites/{{invite_id}}"),
        kind,
        format!("{id}/invites/{{path.invite_id}}"),
        vec![("DELETE", labels(&[&invites]))],
    ));
    identity.push(route(
        format!("{node}/roles"),
        kind,
        format!("{id}/roles"),
        vec![("GET", labels(&[]))],
    ));

    if is_root {
        identity.push(route(
            format!("{node}/members"),
            kind,
            format!("{id}/members"),
            vec![
                ("GET", labels(&[&bindings_read])),
                ("POST", labels(&[&bindings_write])),
            ],
        ));
        identity.push(route(
            format!("{node}/members/{{member}}"),
            kind,
            format!("{id}/members/{{path.member}}"),
            vec![("GET", labels(&[])), ("DELETE", labels(&[&bindings_write]))],
        ));
        identity.push(route(
            format!("{node}/me"),
            kind,
            format!("{id}/members/{{subject.member_id}}"),
            vec![("DELETE", labels(&[]))],
        ));
    }

    audit.push(route(
        format!("{node}/audit-events"),
        kind,
        format!("{id}/audit-events"),
        vec![("GET", labels(&[&l("audit:read")]))],
    ));
}

#[cfg(test)]
mod tests {
    use super::*;
    use crate::config::Config;

    fn kinds() -> Value {
        json!({
            "directory": { "sign_in": "idp" },
            "organization": { "label": "Organization", "segment": "orgs" },
            "folder": { "parent": "organization" },
            "project": { "parent": ["organization", "folder"] }
        })
    }

    fn generated() -> Value {
        let kinds: BTreeMap<String, Kind> = serde_json::from_value(kinds()).unwrap();
        generate(&kinds)
    }

    fn route<'a>(services: &'a Value, service: &str, path: &str) -> &'a Value {
        services[service]["routes"]
            .as_array()
            .unwrap()
            .iter()
            .find(|r| r["path"] == path)
            .unwrap_or_else(|| panic!("no {path}"))
    }

    /// The catalog is a valid config with the kinds it came from: it passes the
    /// schema and every rule, and owns no route twice.
    #[test]
    fn the_catalog_applies() {
        let config = json!({
            "version": 2,
            "kinds": kinds(),
            "roles": { "admin": { "labels": ["*"], "bindable_at": ["organization", "folder", "project"] } },
            "default_role": "admin",
            "creator_role": "admin",
            "services": generated()
        });
        crate::schema::check_config(&config).unwrap();
        let config: Config = serde_json::from_value(config).unwrap();
        crate::rules::check(&config).unwrap();
    }

    #[test]
    fn routes_follow_the_edge_catalog() {
        let s = generated();
        assert_eq!(
            route(&s, "identity", "/orgs"),
            &json!({ "path": "/orgs", "scope": "directory", "upstream": "/roots/organization", "methods": ["POST"] })
        );
        assert_eq!(
            route(&s, "identity", "/orgs/{organization}/projects"),
            &json!({
                "path": "/orgs/{organization}/projects", "scope": "organization",
                "upstream": "/nodes/{subject.organization_id}/children/project",
                "methods": { "GET": { "required_labels": [] }, "POST": { "required_labels": ["project:create"] } }
            })
        );
        // A kind with two parents gets a chain for each.
        route(
            &s,
            "identity",
            "/orgs/{organization}/folders/{folder}/projects/{project}",
        );
        let restore = route(
            &s,
            "identity",
            "/orgs/{organization}/projects/{project}/restore",
        );
        assert_eq!(restore["resolve_deleted"], json!(true));
        assert_eq!(restore["required_labels"], json!(["project:delete"]));
        // Invites below a root admit guests; a root's own admit members.
        assert_eq!(
            route(
                &s,
                "identity",
                "/orgs/{organization}/projects/{project}/invites"
            )["methods"]["GET"],
            json!({ "required_labels": ["organization:guests:write"], "audit": true })
        );
        assert_eq!(
            route(&s, "identity", "/orgs/{organization}/me")["upstream"],
            json!("/nodes/{subject.organization_id}/members/{subject.member_id}")
        );
        assert_eq!(
            route(
                &s,
                "audit",
                "/orgs/{organization}/folders/{folder}/audit-events"
            )["required_labels"],
            json!(["folder:audit:read"])
        );
        assert_eq!(
            route(&s, "audit", "/me/audit-events")["scope"],
            json!("directory")
        );
        // Invite lists return live tokens: audited.
        assert_eq!(
            route(&s, "identity", "/orgs/{organization}/invites")["methods"],
            json!({
                "GET": { "required_labels": ["organization:bindings:write"], "audit": true },
                "POST": { "required_labels": ["organization:bindings:write"] }
            })
        );
        // A derived segment has the slug shape.
        let kinds: BTreeMap<String, Kind> = serde_json::from_value(json!({
            "directory": { "sign_in": "idp" },
            "service_area": {}
        }))
        .unwrap();
        route(
            &generate(&kinds),
            "identity",
            "/service-areas/{service_area}",
        );
        // Only roots get members.
        let paths: Vec<&str> = s["identity"]["routes"]
            .as_array()
            .unwrap()
            .iter()
            .map(|r| r["path"].as_str().unwrap())
            .collect();
        assert!(!paths
            .iter()
            .any(|p| p.ends_with("projects/{project}/members")));
    }
}
