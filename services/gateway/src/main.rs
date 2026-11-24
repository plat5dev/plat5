use std::sync::Arc;

use gateway::auth;
use gateway::config::GatewayConfig;
use gateway::control_plane::ControlPlane;
use gateway::health;
use gateway::metrics;
use gateway::telemetry;
use gateway::Gateway;
use tracing::info;

use pingora::server::configuration::Opt;
use pingora::server::Server;

fn main() {
    let _telemetry_guard = telemetry::init_telemetry().unwrap_or_else(|err| {
        eprintln!("failed to init telemetry: {err}");
        std::process::exit(1);
    });
    metrics::register_process_metrics();

    let cfg = GatewayConfig::from_env().unwrap_or_else(|e| {
        panic!("invalid gateway configuration: {e}");
    });
    info!("gateway configuration loaded");

    // Use a single Tokio runtime for all async initialization.
    let rt = tokio::runtime::Runtime::new().expect("failed to create tokio runtime");

    // Routes, rate limits and CORS: wait for the first snapshot, then follow the
    // control plane.
    let snapshot = rt.block_on(async {
        ControlPlane::connect(&cfg.control_plane_url, telemetry::service_instance_id())
            .await
            .snapshot()
    });

    let opt = Opt::parse_args();
    let mut my_server = Server::new(Some(opt)).unwrap();
    my_server.bootstrap();

    let jwt_validator = auth::jwt::JwtValidatorState::new(
        cfg.auth_issuer.clone(),
        cfg.auth_jwks_uri.clone(),
        cfg.auth_allowed_audiences.clone(),
    );

    // Eagerly fetch JWKS on startup. If it fails we start degraded; the
    // background task retries every 2s until loaded, then every 15 minutes.
    if let Err(err) = rt.block_on(jwt_validator.initialize()) {
        tracing::warn!(error = %err, "failed to fetch JWKS on startup; will retry in background");
    }

    let limiter = if let Some(ref url) = cfg.valkey_url {
        info!(key_prefix = %cfg.valkey_key_prefix, "rate limiting on (fail-open when Valkey unavailable)");
        rt.block_on(gateway::rate_limit::RateLimiter::connect_or_start(
            url,
            cfg.valkey_key_prefix.clone(),
            rt.handle(),
        ))
        .unwrap_or_else(|e| panic!("invalid VALKEY_URL: {e}"))
    } else {
        tracing::warn!("RATE_LIMIT_ENABLED=false: rate limiting is off and Valkey is not used");
        gateway::rate_limit::RateLimiter::disabled()
    };

    if let Some(ref signer) = cfg.upstream_signer {
        info!(kid = %signer.kid(), "upstream request signing on");
    }
    if cfg.upstream_block_private_ips {
        info!("upstream private IP blocking on");
    }

    // Background audit writes run on this runtime, off the request path.
    let audit = gateway::audit::Audit::new(cfg.audit_url.clone(), cfg.audit_mode, rt.handle());
    if let Some(url) = cfg.audit_url.as_deref() {
        info!(audit_url = %url, mode = cfg.audit_mode.as_str(), "audit on");
    } else {
        tracing::warn!("AUDIT_ENABLED=false: audit is off and no audit log is recorded");
    }

    let audit_drain =
        pingora::services::background::background_service("Audit Drain", AuditDrain(audit.clone()));
    my_server.add_service(audit_drain);

    let invalidations = auth::invalidations::InvalidationTracker::new(cfg.admit_cache_ttl);
    invalidations.spawn_follower(cfg.identity_internal_url.clone(), rt.handle());
    let authorizer_invalidator = gateway::authorizer::AuthorizerInvalidator::new();

    let mut my_proxy = pingora::proxy::http_proxy_service(
        &my_server.configuration,
        Gateway::new(
            &cfg,
            jwt_validator.clone(),
            snapshot,
            limiter.clone(),
            audit,
            invalidations,
            authorizer_invalidator.clone(),
        ),
    );
    my_proxy.add_tcp(&format!("0.0.0.0:{}", cfg.port));
    my_server.add_service(my_proxy);

    let health_state = Arc::new(health::HealthState::new(
        jwt_validator,
        limiter,
        authorizer_invalidator,
    ));
    let health_server = health::new_health_server(health_state);
    let mut health_service =
        pingora::services::listening::Service::new("Health and Metrics".to_string(), health_server);
    health_service.add_tcp(&format!("0.0.0.0:{}", cfg.internal_port));
    my_server.add_service(health_service);

    my_server.run_forever();
}

struct AuditDrain(gateway::audit::Audit);

#[async_trait::async_trait]
impl pingora::services::background::BackgroundService for AuditDrain {
    async fn start(&self, mut shutdown: pingora::server::ShutdownWatch) {
        let _ = shutdown.changed().await;
        self.0.drain(std::time::Duration::from_secs(5)).await;
    }
}
