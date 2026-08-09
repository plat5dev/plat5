use std::env;
use std::sync::Arc;

use gateway::auth;
use gateway::health;
use gateway::metrics;
use gateway::route_registry;
use gateway::telemetry;
use gateway::UserGateway;
use tracing::info;

use pingora::server::configuration::Opt;
use pingora::server::Server;

fn main() {
    let _telemetry_guard = telemetry::init_telemetry().expect("failed to initialize telemetry");
    metrics::register_process_metrics();

    // Use a single Tokio runtime for all async initialization.
    let rt = tokio::runtime::Runtime::new().expect("failed to create tokio runtime");

    // Initialize route registry from etcd
    let route_map = rt.block_on(async {
        let registry = route_registry::RouteRegistry::connect()
            .await
            .expect("failed to initialize route registry from etcd");
        registry.route_map()
    });

    info!("route registry initialized");

    // read command line arguments
    let opt = Opt::parse_args();
    let mut my_server = Server::new(Some(opt)).unwrap();
    my_server.bootstrap();

    let issuer = env::var("AUTH_ISSUER").expect("AUTH_ISSUER is not set");
    let jwks_uri = env::var("AUTH_JWKS_URI").expect("AUTH_JWKS_URI is not set");
    let allowed_audiences: Vec<String> = env::var("AUTH_ALLOWED_AUDIENCES")
        .unwrap_or_default()
        .split(',')
        .map(|s| s.trim().to_string())
        .filter(|s| !s.is_empty())
        .collect();
    let user_id_claim = gateway::parse_user_id_claim(
        &env::var("AUTH_USER_ID_CLAIM").unwrap_or_else(|_| "properties.user_id".to_string()),
    );
    let jwt_validator = auth::state::JwtValidatorState::new(issuer, jwks_uri, allowed_audiences);

    // Eagerly fetch JWKS on startup. If it fails we start degraded and the
    // background refresh task will keep retrying.
    if let Err(err) = rt.block_on(jwt_validator.initialize()) {
        tracing::warn!(error = %err, "failed to fetch JWKS on startup; will retry in background");
    }

    let internal_auth_token = env::var("INTERNAL_AUTH_TOKEN")
        .ok()
        .filter(|s| !s.is_empty());

    // Initialize API key validator
    let apikeys_url = env::var("APIKEY_VALIDATE_URL").expect("APIKEY_VALIDATE_URL is not set");
    let apikey_validator =
        auth::apikey::ApiKeyValidator::new(apikeys_url, internal_auth_token.clone());
    let apikey_cache_ttl: Option<u64> = env::var("APIKEY_CACHE_TTL_SECS")
        .ok()
        .and_then(|v| v.parse().ok());

    // Membership resolve (organization scope). Optional: org routes return 503 if unset.
    let membership_resolver = env::var("MEMBERSHIP_RESOLVE_URL")
        .ok()
        .map(|url| auth::membership::MembershipResolver::new(url, internal_auth_token.clone()));
    let membership_cache_ttl: Option<u64> = env::var("MEMBERSHIP_CACHE_TTL_SECS")
        .ok()
        .and_then(|v| v.parse().ok());

    // Configure upstream timeouts
    let upstream_connect_timeout = env::var("UPSTREAM_CONNECT_TIMEOUT_MS")
        .ok()
        .and_then(|v| v.parse().ok())
        .map(std::time::Duration::from_millis)
        .unwrap_or_else(|| std::time::Duration::from_secs(10));
    let upstream_read_timeout = env::var("UPSTREAM_READ_TIMEOUT_MS")
        .ok()
        .and_then(|v| v.parse().ok())
        .map(std::time::Duration::from_millis)
        .unwrap_or_else(|| std::time::Duration::from_secs(30));

    // Empty ALLOWED_ORIGINS → Access-Control-Allow-Origin: *. Non-empty = allowlist.
    let allowed_origins: Vec<String> = env::var("ALLOWED_ORIGINS")
        .unwrap_or_default()
        .split(',')
        .map(|s| s.trim().to_string())
        .filter(|s| !s.is_empty())
        .collect();

    let port = env::var("PORT").unwrap_or_else(|_| "5001".to_string());
    let mut my_proxy = pingora::proxy::http_proxy_service(
        &my_server.configuration,
        UserGateway::new(
            jwt_validator.clone(),
            user_id_claim,
            apikey_validator,
            apikey_cache_ttl,
            membership_resolver,
            membership_cache_ttl,
            route_map,
            upstream_connect_timeout,
            upstream_read_timeout,
            allowed_origins,
        ),
    );
    my_proxy.add_tcp(&format!("0.0.0.0:{}", port));
    my_server.add_service(my_proxy);

    let internal_port = env::var("INTERNAL_PORT").unwrap_or_else(|_| "8000".to_string());
    let health_state = Arc::new(health::HealthState::new(jwt_validator));
    let health_server = health::new_health_server(health_state);
    let mut health_service =
        pingora::services::listening::Service::new("Health and Metrics".to_string(), health_server);
    health_service.add_tcp(&format!("0.0.0.0:{}", internal_port));
    my_server.add_service(health_service);

    my_server.run_forever();
}
