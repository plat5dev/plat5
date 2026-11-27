mod api;
#[cfg(test)]
mod api_tests;
mod apply;
mod auth;
mod config;
mod counts;
#[cfg(test)]
mod docgen;
mod error;
mod expand;
mod feed;
#[cfg(test)]
mod fixtures;
mod metrics;
mod middleware;
mod plan;
mod rules;
mod schema;
mod served;
mod store;
mod telemetry;

use std::net::SocketAddr;
use std::path::PathBuf;
use std::sync::Arc;
use std::time::Duration;

use tracing::{error, info, warn};

use crate::auth::{AuthConfig, Authenticator};
use crate::counts::{Counts, HttpCounts};
use crate::feed::Feed;
use crate::store::Store;

/// How often stale `gateway_replicas` rows (older than 1 hour) are pruned.
const REPLICA_PRUNE_INTERVAL: Duration = Duration::from_secs(300);

#[derive(Clone)]
pub struct AppState {
    pub store: Store,
    pub feed: Feed,
    /// Identity's plan counts, asked only when a plan removes a kind or changes
    /// a kind's parent.
    pub counts: Arc<dyn Counts>,
    pub auth: Authenticator,
}

#[tokio::main]
async fn main() {
    let _telemetry = match telemetry::init_telemetry() {
        Ok(guard) => guard,
        Err(err) => {
            eprintln!("failed to init telemetry: {err}");
            std::process::exit(1);
        }
    };
    metrics::register_process_metrics();

    let database_url = nonempty_env("DATABASE_URL").unwrap_or_else(|| {
        error!("DATABASE_URL is required");
        std::process::exit(1);
    });
    let admin_token = nonempty_env("ADMIN_TOKEN");
    let auth_config = match nonempty_env("AUTH_CONFIG_FILE") {
        Some(path) => match AuthConfig::load_file(&PathBuf::from(&path)) {
            Ok(cfg) => Some(cfg),
            Err(err) => {
                error!(error = %err, "failed to load AUTH_CONFIG_FILE");
                std::process::exit(1);
            }
        },
        None => None,
    };
    if admin_token.is_none() && auth_config.is_none() {
        error!("either ADMIN_TOKEN or AUTH_CONFIG_FILE is required");
        std::process::exit(1);
    }
    if admin_token.is_some() && auth_config.is_some() {
        warn!("static ADMIN_TOKEN is enabled alongside OIDC AUTH_CONFIG_FILE; omit ADMIN_TOKEN in production unless required for break-glass");
    }
    let auth = Authenticator::new(admin_token, auth_config);

    let port: u16 = std::env::var("PORT")
        .ok()
        .and_then(|p| p.parse().ok())
        .unwrap_or(5002);
    let internal_port: u16 = std::env::var("INTERNAL_PORT")
        .ok()
        .and_then(|p| p.parse().ok())
        .unwrap_or(5003);

    let identity_url = nonempty_env("IDENTITY_INTERNAL_URL").unwrap_or_else(|| {
        error!("IDENTITY_INTERNAL_URL is required");
        std::process::exit(1);
    });

    info!("connecting to postgres");
    let store = Store::connect(&database_url).await.unwrap_or_else(|e| {
        error!(error = %e, "failed to connect to postgres");
        std::process::exit(1);
    });

    let counts: Arc<dyn Counts> = Arc::new(HttpCounts::new(&identity_url));

    let (shutdown_tx, shutdown_rx) = tokio::sync::watch::channel(false);
    tokio::spawn(async move {
        wait_for_shutdown_signal().await;
        info!("shutdown signal received");
        let _ = shutdown_tx.send(true);
    });

    let head = store.head().await.unwrap_or_else(|e| {
        error!(error = %e, "failed to read control-plane head");
        std::process::exit(1);
    });
    let feed = Feed::spawn(store.clone(), head, shutdown_rx.clone());
    auth.spawn_refresh(shutdown_rx.clone());

    {
        let store = store.clone();
        let mut shutdown = shutdown_rx.clone();
        tokio::spawn(async move {
            let mut interval = tokio::time::interval(REPLICA_PRUNE_INTERVAL);
            interval.set_missed_tick_behavior(tokio::time::MissedTickBehavior::Delay);
            interval.tick().await;
            loop {
                tokio::select! {
                    _ = feed::wait_shutdown(&mut shutdown) => break,
                    _ = interval.tick() => {}
                }
                if let Err(e) = store.prune_stale_replicas().await {
                    warn!(error = %e, "failed to prune stale gateway replicas");
                }
            }
        });
    }

    let state = AppState {
        store,
        feed,
        counts,
        auth,
    };

    let public = api::public_router(state.clone());
    let internal = api::internal_router(state);

    let public_addr = SocketAddr::from(([0, 0, 0, 0], port));
    let internal_addr = SocketAddr::from(([0, 0, 0, 0], internal_port));

    info!(%public_addr, "control-plane admin API listening");
    info!(%internal_addr, "control-plane internal listening");

    let public_listener = tokio::net::TcpListener::bind(public_addr)
        .await
        .unwrap_or_else(|e| {
            error!(error = %e, "failed to bind public port");
            std::process::exit(1);
        });
    let internal_listener = tokio::net::TcpListener::bind(internal_addr)
        .await
        .unwrap_or_else(|e| {
            error!(error = %e, "failed to bind internal port");
            std::process::exit(1);
        });

    let mut public_rx = shutdown_rx.clone();
    let mut internal_rx = shutdown_rx;
    let public_server = axum::serve(public_listener, public)
        .with_graceful_shutdown(async move { feed::wait_shutdown(&mut public_rx).await });
    let internal_server = axum::serve(internal_listener, internal)
        .with_graceful_shutdown(async move { feed::wait_shutdown(&mut internal_rx).await });

    let (public_result, internal_result) = tokio::join!(public_server, internal_server);
    if let Err(e) = public_result {
        error!(error = %e, "public server error");
    }
    if let Err(e) = internal_result {
        error!(error = %e, "internal server error");
    }
}

fn nonempty_env(key: &str) -> Option<String> {
    std::env::var(key).ok().filter(|v| !v.trim().is_empty())
}

async fn wait_for_shutdown_signal() {
    let ctrl_c = async {
        tokio::signal::ctrl_c()
            .await
            .expect("failed to install Ctrl+C handler");
    };

    #[cfg(unix)]
    let terminate = async {
        tokio::signal::unix::signal(tokio::signal::unix::SignalKind::terminate())
            .expect("failed to install SIGTERM handler")
            .recv()
            .await;
    };

    #[cfg(not(unix))]
    let terminate = std::future::pending::<()>();

    tokio::select! {
        _ = ctrl_c => {},
        _ = terminate => {},
    }
}
