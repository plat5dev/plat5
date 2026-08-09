mod api;
mod error;
mod etcd_store;
mod metrics;
mod route_config;
mod seed;
mod telemetry;

use std::net::SocketAddr;
use std::sync::Arc;

use tracing::{error, info};

use crate::etcd_store::EtcdStore;

#[derive(Clone)]
pub struct AppState {
    pub store: EtcdStore,
    pub admin_token: String,
    pub platform_services: Arc<Vec<String>>,
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

    let etcd_url =
        std::env::var("ETCD_URL").unwrap_or_else(|_| "http://localhost:2379".to_string());
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

    let platform_services: Arc<Vec<String>> = Arc::new(
        std::env::var("PLATFORM_SERVICES")
            .unwrap_or_else(|_| "api-keys,organizations".to_string())
            .split(',')
            .map(|s| s.trim().to_string())
            .filter(|s| !s.is_empty())
            .collect(),
    );

    info!(etcd_url = %etcd_url, "connecting to etcd");
    let store = EtcdStore::connect(&etcd_url).await.unwrap_or_else(|e| {
        error!(error = %e, "failed to connect to etcd");
        std::process::exit(1);
    });

    if let Ok(seed_dir) = std::env::var("SEED_ROUTES_DIR") {
        if !seed_dir.is_empty() {
            seed::seed_from_dir(&store, &seed_dir)
                .await
                .unwrap_or_else(|e| {
                    error!(error = %e, "route seed failed");
                    std::process::exit(1);
                });
        }
    }

    let state = AppState {
        store: store.clone(),
        admin_token,
        platform_services,
    };

    let public = api::public_router(state.clone());
    let internal = api::internal_router(state);

    let public_addr = SocketAddr::from(([0, 0, 0, 0], port));
    let internal_addr = SocketAddr::from(([0, 0, 0, 0], internal_port));

    info!(%public_addr, "route-registry admin API listening");
    info!(%internal_addr, "route-registry internal listening");

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

    let public_server = axum::serve(public_listener, public);
    let internal_server = axum::serve(internal_listener, internal);

    tokio::select! {
        r = public_server => {
            if let Err(e) = r {
                error!(error = %e, "public server error");
            }
        }
        r = internal_server => {
            if let Err(e) = r {
                error!(error = %e, "internal server error");
            }
        }
    }
}
