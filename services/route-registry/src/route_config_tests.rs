#[cfg(test)]
mod tests {
    use super::*;

    fn route(path: &str, methods: &[&str]) -> RouteConfig {
        RouteConfig {
            path: path.to_string(),
            methods: methods.iter().map(|m| m.to_string()).collect(),
            transform: None,
            required_scopes: None,
            rate_limit: None,
            methods_form: MethodsForm::List,
        }
    }

    fn user_service(routes: Vec<RouteConfig>) -> ServiceConfig {
        ServiceConfig {
            url: "w:3000".into(),
            public: None,
            user: Some(ScopeConfig {
                route_prefix: None,
                organization_param: None,
                routes,
            }),
            organization: None,
        }
    }

    fn parse_route(value: serde_json::Value) -> RouteConfig {
        serde_json::from_value(value).expect("route json")
    }

    fn config_with(route: RouteConfig) -> Config {
        let mut services = HashMap::new();
        services.insert("w".into(), user_service(vec![route]));
        Config { services }
    }

    #[test]
    fn join_prefix_root_path() {
        assert_eq!(
            join_route_prefix("/api/organizations", "/").unwrap(),
            "/api/organizations"
        );
    }

    #[test]
    fn join_prefix_subpath() {
        assert_eq!(
            join_route_prefix("/api/organizations", "/{organization_id}").unwrap(),
            "/api/organizations/{organization_id}"
        );
    }

    #[test]
    fn prepare_expands_prefix() {
        let svc = ServiceConfig {
            url: "orgs:3000".into(),
            public: None,
            user: Some(ScopeConfig {
                route_prefix: Some("/api/organizations".into()),
                organization_param: None,
                routes: vec![route("/", &["GET"])],
            }),
            organization: None,
        };
        let prepared = svc.prepare_for_registry("organizations").unwrap();
        assert!(prepared.user.as_ref().unwrap().route_prefix.is_none());
        assert_eq!(
            prepared.user.as_ref().unwrap().routes[0].path,
            "/api/organizations"
        );
    }

    #[test]
    fn required_scopes_ok() {
        let mut r = route("/api/widgets", &["GET"]);
        r.required_scopes = Some(vec!["widgets:read".into(), "invoices.write".into()]);
        config_with(r).validate().unwrap();
    }

    #[test]
    fn required_scopes_reject_empty_and_bad() {
        for labels in [
            vec![],
            vec!["Widgets:read".into()],
            vec!["bad/scope".into()],
            vec!["ok".into(), "ok".into()],
        ] {
            let mut r = route("/api/widgets", &["GET"]);
            r.required_scopes = Some(labels.clone());
            assert!(
                config_with(r).validate().is_err(),
                "expected error for {labels:?}"
            );
        }
    }

    #[test]
    fn rate_limit_false_and_object() {
        let unlimited: RouteRateLimit = serde_json::from_str("false").unwrap();
        assert_eq!(unlimited, RouteRateLimit::Unlimited);
        let obj: RouteRateLimit = serde_json::from_value(serde_json::json!({
            "requests": 10,
            "window_seconds": 60
        }))
        .unwrap();
        match obj {
            RouteRateLimit::Limit(cfg) => {
                assert_eq!(cfg.requests, 10);
                assert_eq!(cfg.window_seconds, 60);
                assert!(cfg.by.is_none());
            }
            RouteRateLimit::Unlimited => panic!("expected object"),
        }
        let with_by: RouteRateLimit = serde_json::from_value(serde_json::json!({
            "requests": 10,
            "window_seconds": 60,
            "by": "ip"
        }))
        .unwrap();
        match with_by {
            RouteRateLimit::Limit(cfg) => assert_eq!(cfg.by.as_deref(), Some("ip")),
            RouteRateLimit::Unlimited => panic!("expected object"),
        }
        assert!(serde_json::from_str::<RouteRateLimit>("true").is_err());
    }

    #[test]
    fn rate_limit_object_must_be_positive() {
        let mut r = route("/api/widgets", &["GET"]);
        r.rate_limit = Some(RouteRateLimit::Limit(RateLimitConfig {
            requests: 0,
            window_seconds: 60,
            by: None,
        }));
        assert!(config_with(r).validate().is_err());
    }

    #[test]
    fn rate_limit_by_is_rejected() {
        for by in ["key", "member", "org", "ip", "user"] {
            let mut r = route("/api/widgets", &["GET"]);
            r.rate_limit = Some(RouteRateLimit::Limit(RateLimitConfig {
                requests: 10,
                window_seconds: 60,
                by: Some(by.into()),
            }));
            let err = config_with(r).validate().unwrap_err();
            let msg = err.to_string();
            assert!(
                msg.contains("rate_limit.by"),
                "expected by rejection for {by}, got {msg}"
            );
        }
    }

    #[test]
    fn nested_rate_limit_by_is_rejected() {
        for by in ["key", "member", "org", "ip"] {
            let r = parse_route(serde_json::json!({
                "path": "/features",
                "methods": {
                    "POST": {
                        "rate_limit": {"requests": 10, "window_seconds": 60, "by": by}
                    }
                }
            }));
            let err = config_with(r).validate().unwrap_err();
            let msg = err.to_string();
            assert!(
                msg.contains("rate_limit.by"),
                "expected nested by rejection for {by}, got {msg}"
            );
        }
    }

    #[test]
    fn rate_limit_object_without_by_ok() {
        let mut r = route("/api/widgets", &["GET"]);
        r.rate_limit = Some(RouteRateLimit::Limit(RateLimitConfig {
            requests: 10,
            window_seconds: 60,
            by: None,
        }));
        config_with(r).validate().unwrap();
    }

    #[test]
    fn scopes_intersect_nonempty() {
        assert!(scopes_intersect(
            &["a".into(), "b".into()],
            &["b".into(), "c".into()]
        ));
        assert!(!scopes_intersect(&["a".into()], &[]));
        assert!(!scopes_intersect(&["a".into()], &["b".into()]));
    }

    #[test]
    fn flat_methods_list_still_validates() {
        let r = parse_route(serde_json::json!({
            "path": "/features",
            "methods": ["GET", "POST"],
            "required_scopes": ["org:read"]
        }));
        assert_eq!(r.methods, vec!["GET", "POST"]);
        assert_eq!(
            r.required_scopes.as_deref(),
            Some(["org:read".to_string()].as_slice())
        );
        assert!(matches!(r.methods_form, MethodsForm::List));
        config_with(r).validate().unwrap();
    }

    #[test]
    fn nested_get_post_different_scopes_expands_to_two_routes() {
        let r = parse_route(serde_json::json!({
            "path": "/features",
            "methods": {
                "GET": {"required_scopes": ["org:read"]},
                "POST": {
                    "required_scopes": ["org:write"],
                    "rate_limit": {"requests": 100, "window_seconds": 1}
                }
            }
        }));
        let prepared = user_service(vec![r])
            .prepare_for_registry("w")
            .expect("expand nested methods");
        let routes = &prepared.user.as_ref().unwrap().routes;
        assert_eq!(routes.len(), 2);
        let get = routes
            .iter()
            .find(|rt| rt.methods == ["GET"])
            .expect("GET row");
        assert_eq!(get.path, "/features");
        assert_eq!(
            get.required_scopes.as_deref(),
            Some(["org:read".to_string()].as_slice())
        );
        assert!(get.rate_limit.is_none());
        assert!(matches!(get.methods_form, MethodsForm::List));
        let post = routes
            .iter()
            .find(|rt| rt.methods == ["POST"])
            .expect("POST row");
        assert_eq!(
            post.required_scopes.as_deref(),
            Some(["org:write".to_string()].as_slice())
        );
        match &post.rate_limit {
            Some(RouteRateLimit::Limit(cfg)) => {
                assert_eq!(cfg.requests, 100);
                assert_eq!(cfg.window_seconds, 1);
                assert!(cfg.by.is_none());
            }
            other => panic!("expected POST rate limit, got {other:?}"),
        }
        let json = serde_json::to_value(get).unwrap();
        assert!(json["methods"].is_array(), "etcd methods must stay a list");
        assert!(json.get("methods_form").is_none());
    }

    #[test]
    fn mixing_list_and_map_fails() {
        let r = parse_route(serde_json::json!({
            "path": "/features",
            "methods": ["GET", {"POST": {}}]
        }));
        let err = config_with(r).validate().unwrap_err();
        let msg = err.to_string();
        assert!(msg.contains("mix"), "expected mix error, got {msg}");
    }

    #[test]
    fn empty_nested_methods_map_fails() {
        let r = parse_route(serde_json::json!({"path": "/features", "methods": {}}));
        let err = config_with(r).validate().unwrap_err();
        let msg = err.to_string();
        assert!(
            msg.contains("methods map must not be empty") || msg.contains("no methods"),
            "expected empty map error, got {msg}"
        );
    }

    #[test]
    fn nested_method_empty_body_ok() {
        let r = parse_route(serde_json::json!({
            "path": "/features",
            "methods": {"GET": null, "POST": {}}
        }));
        let prepared = user_service(vec![r])
            .prepare_for_registry("w")
            .expect("empty method body is valid");
        let routes = &prepared.user.as_ref().unwrap().routes;
        assert_eq!(routes.len(), 2);
        for rt in routes {
            assert!(rt.required_scopes.is_none());
            assert!(rt.rate_limit.is_none());
            assert_eq!(rt.methods.len(), 1);
        }
    }

    #[test]
    fn duplicate_path_method_after_expand_fails() {
        let nested = parse_route(serde_json::json!({
            "path": "/features",
            "methods": {"GET": {"required_scopes": ["org:read"]}}
        }));
        let flat = parse_route(serde_json::json!({
            "path": "/features",
            "methods": ["GET"]
        }));
        let err = user_service(vec![nested, flat])
            .prepare_for_registry("w")
            .unwrap_err();
        let msg = err.to_string();
        assert!(
            msg.contains("duplicate"),
            "expected duplicate path+method, got {msg}"
        );
    }

    #[test]
    fn yaml_nested_empty_body_ok() {
        let yaml = "path: /features\nmethods:\n  GET:\n  POST: {}\n";
        let r: RouteConfig = serde_yaml::from_str(yaml).expect("yaml empty method body");
        let prepared = user_service(vec![r])
            .prepare_for_registry("w")
            .expect("yaml empty method body is valid");
        let routes = &prepared.user.as_ref().unwrap().routes;
        assert_eq!(routes.len(), 2);
        for rt in routes {
            assert!(rt.required_scopes.is_none());
            assert!(rt.rate_limit.is_none());
            assert_eq!(rt.methods.len(), 1);
            assert!(matches!(rt.methods_form, MethodsForm::List));
        }
    }
}
