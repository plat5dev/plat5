use std::sync::Arc;

use gateway::auth;
use gateway::config::GatewayConfig;
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

    let cfg = GatewayConfig::from_env().unwrap_or_else(|e| {
        panic!("invalid gateway configuration: {e}");
    });

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

    let jwt_validator = auth::jwt::JwtValidatorState::new(
        cfg.auth_issuer.clone(),
        cfg.auth_jwks_uri.clone(),
        cfg.auth_allowed_audiences.clone(),
    );

    // Eagerly fetch JWKS on startup. If it fails we start degraded and the
    // background refresh task will keep retrying.
    if let Err(err) = rt.block_on(jwt_validator.initialize()) {
        tracing::warn!(error = %err, "failed to fetch JWKS on startup; will retry in background");
    }

    let mut my_proxy = pingora::proxy::http_proxy_service(
        &my_server.configuration,
        UserGateway::new(&cfg, jwt_validator.clone(), route_map),
    );
    my_proxy.add_tcp(&format!("0.0.0.0:{}", cfg.port));
    my_server.add_service(my_proxy);

    let health_state = Arc::new(health::HealthState::new(jwt_validator));
    let health_server = health::new_health_server(health_state);
    let mut health_service =
        pingora::services::listening::Service::new("Health and Metrics".to_string(), health_server);
    health_service.add_tcp(&format!("0.0.0.0:{}", cfg.internal_port));
    my_server.add_service(health_service);

    my_server.run_forever();
}
