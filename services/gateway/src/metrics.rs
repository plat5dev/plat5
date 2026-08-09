use once_cell::sync::Lazy;
use opentelemetry::metrics::{Counter, Histogram};
use opentelemetry::{global, KeyValue};
use prometheus::{
    histogram_opts, register_histogram_vec, register_int_counter_vec, HistogramVec, IntCounterVec,
};

/// Registers process metrics (CPU, memory, file descriptors, etc.)
/// Must be called once at startup. Only available on Linux.
#[cfg(target_os = "linux")]
pub fn register_process_metrics() {
    use prometheus::process_collector::ProcessCollector;
    let collector = ProcessCollector::for_self();
    if let Err(err) = prometheus::register(Box::new(collector)) {
        tracing::warn!(
            ?err,
            "failed to register process collector (may already be registered)"
        );
    }
}

/// No-op on non-Linux platforms (process metrics require procfs)
#[cfg(not(target_os = "linux"))]
pub fn register_process_metrics() {}

pub static HTTP_REQUESTS_TOTAL: Lazy<IntCounterVec> = Lazy::new(|| {
    register_int_counter_vec!(
        "http_requests_total",
        "Total HTTP requests processed by the gateway",
        &["route", "method", "status"]
    )
    .expect("failed to register http_requests_total")
});

pub static HTTP_REQUEST_DURATION_SECONDS: Lazy<HistogramVec> = Lazy::new(|| {
    let opts = histogram_opts!(
        "http_request_duration_seconds",
        "HTTP request latency as observed by the gateway",
        vec![0.005, 0.01, 0.025, 0.05, 0.1, 0.25, 0.5, 1.0, 2.5, 5.0]
    );
    register_histogram_vec!(opts, &["route", "method"])
        .expect("failed to register http_request_duration_seconds")
});

pub static AUTH_FAILURES_TOTAL: Lazy<IntCounterVec> = Lazy::new(|| {
    register_int_counter_vec!(
        "auth_failures_total",
        "Authentication failures observed by the gateway",
        &["auth_type", "reason"]
    )
    .expect("failed to register auth_failures_total")
});

pub static AUTH_VALIDATION_DURATION_SECONDS: Lazy<HistogramVec> = Lazy::new(|| {
    let opts = histogram_opts!(
        "auth_validation_duration_seconds",
        "Latency of authentication validation operations",
        vec![0.001, 0.005, 0.01, 0.025, 0.05, 0.1, 0.25, 0.5, 1.0]
    );
    register_histogram_vec!(opts, &["auth_type", "outcome"])
        .expect("failed to register auth_validation_duration_seconds")
});

pub static AUTH_CACHE_HITS_TOTAL: Lazy<IntCounterVec> = Lazy::new(|| {
    register_int_counter_vec!(
        "auth_cache_hits_total",
        "Authentication cache hits",
        &["auth_type"]
    )
    .expect("failed to register auth_cache_hits_total")
});

pub static AUTH_CACHE_MISSES_TOTAL: Lazy<IntCounterVec> = Lazy::new(|| {
    register_int_counter_vec!(
        "auth_cache_misses_total",
        "Authentication cache misses",
        &["auth_type"]
    )
    .expect("failed to register auth_cache_misses_total")
});

// OTEL dual-write instruments. No-op until a MeterProvider is installed.
struct OtelInstruments {
    http_requests: Counter<u64>,
    http_duration: Histogram<f64>,
    auth_failures: Counter<u64>,
    auth_validation: Histogram<f64>,
    auth_cache_hits: Counter<u64>,
    auth_cache_misses: Counter<u64>,
}

static OTEL: Lazy<OtelInstruments> = Lazy::new(|| {
    let meter = global::meter("gateway");
    OtelInstruments {
        http_requests: meter
            .u64_counter("http_requests_total")
            .with_description("Total HTTP requests processed by the gateway")
            .build(),
        http_duration: meter
            .f64_histogram("http_request_duration_seconds")
            .with_description("HTTP request latency as observed by the gateway")
            .with_boundaries(vec![
                0.005, 0.01, 0.025, 0.05, 0.1, 0.25, 0.5, 1.0, 2.5, 5.0,
            ])
            .build(),
        auth_failures: meter
            .u64_counter("auth_failures_total")
            .with_description("Authentication failures observed by the gateway")
            .build(),
        auth_validation: meter
            .f64_histogram("auth_validation_duration_seconds")
            .with_description("Latency of authentication validation operations")
            .with_boundaries(vec![0.001, 0.005, 0.01, 0.025, 0.05, 0.1, 0.25, 0.5, 1.0])
            .build(),
        auth_cache_hits: meter
            .u64_counter("auth_cache_hits_total")
            .with_description("Authentication cache hits")
            .build(),
        auth_cache_misses: meter
            .u64_counter("auth_cache_misses_total")
            .with_description("Authentication cache misses")
            .build(),
    }
});

/// Force OTEL instrument creation after MeterProvider is installed.
pub fn init_otel_instruments() {
    Lazy::force(&OTEL);
}

pub fn record_request(route: &str, method: &str, status: u16, duration_secs: f64) {
    let status_s = status.to_string();
    HTTP_REQUESTS_TOTAL
        .with_label_values(&[route, method, &status_s])
        .inc();
    HTTP_REQUEST_DURATION_SECONDS
        .with_label_values(&[route, method])
        .observe(duration_secs);

    let attrs = [
        KeyValue::new("route", route.to_string()),
        KeyValue::new("method", method.to_string()),
        KeyValue::new("status", status_s),
    ];
    OTEL.http_requests.add(1, &attrs);
    OTEL.http_duration.record(duration_secs, &attrs[..2]);
}

pub fn record_auth_failure(auth_type: &str, reason: &str) {
    AUTH_FAILURES_TOTAL
        .with_label_values(&[auth_type, reason])
        .inc();
    OTEL.auth_failures.add(
        1,
        &[
            KeyValue::new("auth_type", auth_type.to_string()),
            KeyValue::new("reason", reason.to_string()),
        ],
    );
}

pub fn record_auth_validation(auth_type: &str, outcome: &str, duration_secs: f64) {
    AUTH_VALIDATION_DURATION_SECONDS
        .with_label_values(&[auth_type, outcome])
        .observe(duration_secs);
    OTEL.auth_validation.record(
        duration_secs,
        &[
            KeyValue::new("auth_type", auth_type.to_string()),
            KeyValue::new("outcome", outcome.to_string()),
        ],
    );
}

pub fn record_auth_cache_hit(auth_type: &str) {
    AUTH_CACHE_HITS_TOTAL.with_label_values(&[auth_type]).inc();
    OTEL.auth_cache_hits
        .add(1, &[KeyValue::new("auth_type", auth_type.to_string())]);
}

pub fn record_auth_cache_miss(auth_type: &str) {
    AUTH_CACHE_MISSES_TOTAL
        .with_label_values(&[auth_type])
        .inc();
    OTEL.auth_cache_misses
        .add(1, &[KeyValue::new("auth_type", auth_type.to_string())]);
}
