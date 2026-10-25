mod api;
#[cfg(test)]
mod api_tests;
mod apply;
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
mod plan;
mod resources;
mod rules;
mod schema;
mod store;
mod telemetry;

use std::net::SocketAddr;
use std::sync::Arc;

use tracing::{error, info};

use crate::counts::{Counts, HttpCounts};
use crate::feed::Feed;
use crate::store::Store;

#[derive(Clone)]
pub struct AppState {
    pub store: Store,
    pub feed: Feed,
    /// Identity's plan counts, asked only when a plan removes a kind or changes
    /// a kind's parent.
    pub counts: Arc<dyn Counts>,
    pub admin_token: String,
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

    let database_url = std::env::var("DATABASE_URL").unwrap_or_else(|_| {
        error!("DATABASE_URL is required");
        std::process::exit(1);
    });
    if database_url.is_empty() {
        error!("DATABASE_URL must not be empty");
        std::process::exit(1);
    }
    let admin_token = std::env::var("ADMIN_TOKEN").unwrap_or_else(|_| {
        error!("ADMIN_TOKEN is required");
        std::process::exit(1);
    });
    if admin_token.is_empty() {
        error!("ADMIN_TOKEN must not be empty");
        std::process::exit(1);
    }

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

    let head = store.head().await.unwrap_or_else(|e| {
        error!(error = %e, "failed to read control-plane head");
        std::process::exit(1);
    });
    let feed = Feed::spawn(store.clone(), head);

    let state = AppState {
        store,
        feed,
        counts,
        admin_token,
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

    let (shutdown_tx, shutdown_rx) = tokio::sync::watch::channel(false);
    tokio::spawn(async move {
        wait_for_shutdown_signal().await;
        info!("shutdown signal received");
        let _ = shutdown_tx.send(true);
    });

    let public_shutdown = watch_shutdown(shutdown_rx.clone());
    let internal_shutdown = watch_shutdown(shutdown_rx);

    let public_server =
        axum::serve(public_listener, public).with_graceful_shutdown(public_shutdown);
    let internal_server =
        axum::serve(internal_listener, internal).with_graceful_shutdown(internal_shutdown);

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

async fn watch_shutdown(mut rx: tokio::sync::watch::Receiver<bool>) {
    loop {
        if *rx.borrow_and_update() {
            return;
        }
        if rx.changed().await.is_err() {
            return;
        }
    }
}
