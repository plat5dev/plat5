use once_cell::sync::Lazy;
use opentelemetry::metrics::{Counter, Histogram};
use opentelemetry::{global, KeyValue};
use prometheus::{
    histogram_opts, register_histogram_vec, register_int_counter_vec, Encoder, HistogramVec,
    IntCounterVec, TextEncoder,
};

/// Registers process metrics (CPU, memory, start time, etc.).
/// Linux only — process collector needs procfs (prod containers are Linux).
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

#[cfg(not(target_os = "linux"))]
pub fn register_process_metrics() {}

static HTTP_REQUESTS_TOTAL: Lazy<IntCounterVec> = Lazy::new(|| {
    register_int_counter_vec!(
        "http_requests_total",
        "Total HTTP requests processed by route-registry",
        &["route", "method", "status"]
    )
    .expect("failed to register http_requests_total")
});

static HTTP_REQUEST_DURATION_SECONDS: Lazy<HistogramVec> = Lazy::new(|| {
    let opts = histogram_opts!(
        "http_request_duration_seconds",
        "HTTP request latency as observed by route-registry",
        vec![0.005, 0.01, 0.025, 0.05, 0.1, 0.25, 0.5, 1.0, 2.5, 5.0]
    );
    register_histogram_vec!(opts, &["route", "method"])
        .expect("failed to register http_request_duration_seconds")
});

// OTel dual-write — no-op until a MeterProvider is installed (metrics OTLP on).
struct OtelInstruments {
    http_requests: Counter<u64>,
    http_duration: Histogram<f64>,
}

static OTEL: Lazy<OtelInstruments> = Lazy::new(|| {
    let meter = global::meter("route-registry");
    OtelInstruments {
        http_requests: meter
            .u64_counter("http_requests_total")
            .with_description("Total HTTP requests processed by route-registry")
            .build(),
        http_duration: meter
            .f64_histogram("http_request_duration_seconds")
            .with_description("HTTP request latency as observed by route-registry")
            .with_boundaries(vec![
                0.005, 0.01, 0.025, 0.05, 0.1, 0.25, 0.5, 1.0, 2.5, 5.0,
            ])
            .build(),
    }
});

/// Ensure prometheus series exist before the first scrape.
pub fn init() {
    Lazy::force(&HTTP_REQUESTS_TOTAL);
    Lazy::force(&HTTP_REQUEST_DURATION_SECONDS);
}

/// Force OTel instrument creation after MeterProvider is installed.
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

/// Prometheus text exposition for `GET /metrics`.
pub fn gather_text() -> (String, Vec<u8>) {
    init();
    let encoder = TextEncoder::new();
    let metric_families = prometheus::gather();
    let mut buffer = Vec::new();
    encoder
        .encode(&metric_families, &mut buffer)
        .expect("encode prometheus metrics");
    (encoder.format_type().to_string(), buffer)
}
