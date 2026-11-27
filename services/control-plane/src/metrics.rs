use opentelemetry::metrics::{Counter, Histogram, ObservableCounter, ObservableGauge};
use opentelemetry::{global, KeyValue};
use prometheus::{
    histogram_opts, register_histogram_vec, register_int_counter_vec, Encoder, HistogramVec,
    IntCounterVec, TextEncoder,
};
use std::sync::LazyLock;

/// Registers process metrics (CPU, memory, start time, etc.).
/// Linux only — process collector needs procfs (prod containers are Linux).
#[cfg(target_os = "linux")]
pub fn register_process_metrics() {
    use prometheus::process_collector::ProcessCollector;
    let collector = ProcessCollector::for_self();
    if let Err(err) = prometheus::register(Box::new(collector)) {
        tracing::warn!(
            error = ?err,
            "failed to register process collector (may already be registered)"
        );
    }
}

#[cfg(not(target_os = "linux"))]
pub fn register_process_metrics() {}

static HTTP_REQUESTS_TOTAL: LazyLock<IntCounterVec> = LazyLock::new(|| {
    register_int_counter_vec!(
        "http_requests_total",
        "Total HTTP requests processed by control-plane",
        &["route", "method", "status"]
    )
    .expect("failed to register http_requests_total")
});

static CONTROL_PLANE_AUTH_FAILURES_TOTAL: LazyLock<IntCounterVec> = LazyLock::new(|| {
    register_int_counter_vec!(
        "control_plane_auth_failures_total",
        "Total failed control-plane admin API auth attempts",
        &["reason"]
    )
    .expect("failed to register control_plane_auth_failures_total")
});

static HTTP_REQUEST_DURATION_SECONDS: LazyLock<HistogramVec> = LazyLock::new(|| {
    let opts = histogram_opts!(
        "http_request_duration_seconds",
        "HTTP request latency as observed by control-plane",
        vec![0.005, 0.01, 0.025, 0.05, 0.1, 0.25, 0.5, 1.0, 2.5, 5.0]
    );
    register_histogram_vec!(opts, &["route", "method"])
        .expect("failed to register http_request_duration_seconds")
});

// OTel dual-write — no-op until a MeterProvider is installed (metrics OTLP on).
struct OtelInstruments {
    http_requests: Counter<u64>,
    http_duration: Histogram<f64>,
    auth_failures: Counter<u64>,
    _process_start_time: ObservableGauge<f64>,
    _process_cpu_seconds: ObservableCounter<f64>,
    _process_resident_memory: ObservableGauge<f64>,
}

fn gather_prom_scalar(name: &str) -> Option<f64> {
    for mf in prometheus::default_registry().gather() {
        if mf.get_name() == name {
            if let Some(m) = mf.get_metric().first() {
                return match mf.get_field_type() {
                    prometheus::proto::MetricType::GAUGE => Some(m.get_gauge().get_value()),
                    prometheus::proto::MetricType::COUNTER => Some(m.get_counter().get_value()),
                    _ => None,
                };
            }
        }
    }
    None
}

static OTEL: LazyLock<OtelInstruments> = LazyLock::new(|| {
    let meter = global::meter("control-plane");
    OtelInstruments {
        http_requests: meter
            .u64_counter("http_requests_total")
            .with_description("Total HTTP requests processed by control-plane")
            .build(),
        http_duration: meter
            .f64_histogram("http_request_duration_seconds")
            .with_description("HTTP request latency as observed by control-plane")
            .with_boundaries(vec![
                0.005, 0.01, 0.025, 0.05, 0.1, 0.25, 0.5, 1.0, 2.5, 5.0,
            ])
            .build(),
        auth_failures: meter
            .u64_counter("control_plane_auth_failures_total")
            .with_description("Total failed control-plane admin API auth attempts")
            .build(),
        _process_start_time: meter
            .f64_observable_gauge("process_start_time_seconds")
            .with_description("Start time of the process since unix epoch in seconds")
            .with_callback(|observer| {
                if let Some(v) = gather_prom_scalar("process_start_time_seconds") {
                    observer.observe(v, &[]);
                }
            })
            .build(),
        _process_cpu_seconds: meter
            .f64_observable_counter("process_cpu_seconds_total")
            .with_description("Total user and system CPU time spent in seconds")
            .with_callback(|observer| {
                if let Some(v) = gather_prom_scalar("process_cpu_seconds_total") {
                    observer.observe(v, &[]);
                }
            })
            .build(),
        _process_resident_memory: meter
            .f64_observable_gauge("process_resident_memory_bytes")
            .with_description("Resident memory size in bytes")
            .with_callback(|observer| {
                if let Some(v) = gather_prom_scalar("process_resident_memory_bytes") {
                    observer.observe(v, &[]);
                }
            })
            .build(),
    }
});

/// Ensure prometheus series exist before the first scrape.
pub fn init() {
    LazyLock::force(&HTTP_REQUESTS_TOTAL);
    LazyLock::force(&HTTP_REQUEST_DURATION_SECONDS);
    LazyLock::force(&CONTROL_PLANE_AUTH_FAILURES_TOTAL);
}

pub fn record_auth_failure(reason: &str) {
    CONTROL_PLANE_AUTH_FAILURES_TOTAL
        .with_label_values(&[reason])
        .inc();
    OTEL.auth_failures
        .add(1, &[KeyValue::new("reason", reason.to_string())]);
}

/// Force OTel instrument creation after MeterProvider is installed.
pub fn init_otel_instruments() {
    LazyLock::force(&OTEL);
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
