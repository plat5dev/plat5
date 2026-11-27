use opentelemetry::metrics::{Counter, Gauge, Histogram, ObservableCounter, ObservableGauge};
use opentelemetry::{global, KeyValue};
use prometheus::{
    histogram_opts, register_histogram_vec, register_int_counter_vec, register_int_gauge,
    HistogramVec, IntCounterVec, IntGauge,
};
use std::sync::atomic::{AtomicBool, Ordering};
use std::sync::LazyLock;

/// Registers process metrics (CPU, memory, file descriptors, etc.)
/// Must be called once at startup. Only available on Linux.
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

/// No-op on non-Linux platforms (process metrics require procfs)
#[cfg(not(target_os = "linux"))]
pub fn register_process_metrics() {}

pub static HTTP_REQUESTS_TOTAL: LazyLock<IntCounterVec> = LazyLock::new(|| {
    register_int_counter_vec!(
        "http_requests_total",
        "Total HTTP requests processed by the gateway",
        &["route", "method", "status"]
    )
    .expect("failed to register http_requests_total")
});

pub static HTTP_REQUEST_DURATION_SECONDS: LazyLock<HistogramVec> = LazyLock::new(|| {
    let opts = histogram_opts!(
        "http_request_duration_seconds",
        "HTTP request latency as observed by the gateway",
        vec![0.005, 0.01, 0.025, 0.05, 0.1, 0.25, 0.5, 1.0, 2.5, 5.0]
    );
    register_histogram_vec!(opts, &["route", "method"])
        .expect("failed to register http_request_duration_seconds")
});

pub static AUTH_FAILURES_TOTAL: LazyLock<IntCounterVec> = LazyLock::new(|| {
    register_int_counter_vec!(
        "auth_failures_total",
        "Authentication failures observed by the gateway",
        &["auth_type", "reason"]
    )
    .expect("failed to register auth_failures_total")
});

pub static AUTH_VALIDATION_DURATION_SECONDS: LazyLock<HistogramVec> = LazyLock::new(|| {
    let opts = histogram_opts!(
        "auth_validation_duration_seconds",
        "Latency of authentication validation operations",
        vec![0.001, 0.005, 0.01, 0.025, 0.05, 0.1, 0.25, 0.5, 1.0]
    );
    register_histogram_vec!(opts, &["auth_type", "outcome"])
        .expect("failed to register auth_validation_duration_seconds")
});

pub static AUTH_CACHE_HITS_TOTAL: LazyLock<IntCounterVec> = LazyLock::new(|| {
    register_int_counter_vec!(
        "auth_cache_hits_total",
        "Authentication cache hits",
        &["auth_type"]
    )
    .expect("failed to register auth_cache_hits_total")
});

pub static AUTH_CACHE_MISSES_TOTAL: LazyLock<IntCounterVec> = LazyLock::new(|| {
    register_int_counter_vec!(
        "auth_cache_misses_total",
        "Authentication cache misses",
        &["auth_type"]
    )
    .expect("failed to register auth_cache_misses_total")
});

pub static AUDIT_WRITES_TOTAL: LazyLock<IntCounterVec> = LazyLock::new(|| {
    register_int_counter_vec!(
        "audit_writes_total",
        "Audit writes to the audit service by write (intent, outcome, rejected, event) and result",
        &["write", "result"]
    )
    .expect("failed to register audit_writes_total")
});

pub static AUDIT_INTENT_DURATION_SECONDS: LazyLock<HistogramVec> = LazyLock::new(|| {
    let opts = histogram_opts!(
        "audit_intent_duration_seconds",
        "Time an audited request waited on its intent write, retries included",
        vec![0.001, 0.005, 0.01, 0.025, 0.05, 0.1, 0.25, 0.5, 1.0]
    );
    register_histogram_vec!(opts, &["result"])
        .expect("failed to register audit_intent_duration_seconds")
});

pub static GATEWAY_CONFIG_REVISION: LazyLock<IntGauge> = LazyLock::new(|| {
    register_int_gauge!(
        "gateway_config_revision",
        "The control-plane revision this replica serves"
    )
    .expect("failed to register gateway_config_revision")
});

pub static GATEWAY_CONFIG_REFUSED_TOTAL: LazyLock<IntCounterVec> = LazyLock::new(|| {
    register_int_counter_vec!(
        "gateway_config_refused_total",
        "Control-plane snapshots this replica refused, by revision",
        &["revision"]
    )
    .expect("failed to register gateway_config_refused_total")
});

pub static RATE_LIMIT_FAIL_OPEN_TOTAL: LazyLock<IntCounterVec> = LazyLock::new(|| {
    register_int_counter_vec!(
        "rate_limit_fail_open_total",
        "Rate-limit checks allowed fail-open while Valkey was unavailable, by reason",
        &["reason"]
    )
    .expect("failed to register rate_limit_fail_open_total")
});

pub static VALKEY_UP: LazyLock<IntGauge> = LazyLock::new(|| {
    register_int_gauge!(
        "valkey_up",
        "1 when the gateway's Valkey connection is healthy, 0 when failing open"
    )
    .expect("failed to register valkey_up")
});

pub static AUDIT_QUEUE_DEPTH: LazyLock<IntGauge> = LazyLock::new(|| {
    register_int_gauge!(
        "audit_queue_depth",
        "Queued background audit writes waiting to be sent to the audit service"
    )
    .expect("failed to register audit_queue_depth")
});

pub static ADMIT_INVALIDATIONS_TOTAL: LazyLock<IntCounterVec> = LazyLock::new(|| {
    register_int_counter_vec!(
        "admit_invalidations_total",
        "Admit cache invalidations received from identity, by scope",
        &["scope"]
    )
    .expect("failed to register admit_invalidations_total")
});

pub static AUTHORIZER_REQUESTS_TOTAL: LazyLock<IntCounterVec> = LazyLock::new(|| {
    register_int_counter_vec!(
        "authorizer_requests_total",
        "Outbound AuthZEN PDP requests by authorizer, operation and result",
        &["authorizer", "operation", "result"]
    )
    .expect("failed to register authorizer_requests_total")
});

pub static AUTHORIZER_DURATION_SECONDS: LazyLock<HistogramVec> = LazyLock::new(|| {
    let opts = histogram_opts!(
        "authorizer_duration_seconds",
        "Latency of outbound AuthZEN PDP requests",
        vec![0.001, 0.005, 0.01, 0.025, 0.05, 0.1, 0.25, 0.5, 1.0, 2.5]
    );
    register_histogram_vec!(opts, &["authorizer", "operation"])
        .expect("failed to register authorizer_duration_seconds")
});

pub static AUTHORIZER_CACHE_TOTAL: LazyLock<IntCounterVec> = LazyLock::new(|| {
    register_int_counter_vec!(
        "authorizer_cache_total",
        "Outbound AuthZEN decision cache lookups by authorizer and result (hit, miss)",
        &["authorizer", "result"]
    )
    .expect("failed to register authorizer_cache_total")
});

// OTEL dual-write instruments. No-op until a MeterProvider is installed.
struct OtelInstruments {
    http_requests: Counter<u64>,
    http_duration: Histogram<f64>,
    auth_failures: Counter<u64>,
    auth_validation: Histogram<f64>,
    auth_cache_hits: Counter<u64>,
    auth_cache_misses: Counter<u64>,
    audit_writes: Counter<u64>,
    audit_intent_duration: Histogram<f64>,
    audit_queue_depth: Gauge<i64>,
    config_revision: Gauge<i64>,
    config_refused: Counter<u64>,
    rate_limit_fail_open: Counter<u64>,
    valkey_up: Gauge<i64>,
    admit_invalidations: Counter<u64>,
    authorizer_requests: Counter<u64>,
    authorizer_duration: Histogram<f64>,
    authorizer_cache: Counter<u64>,
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
        audit_writes: meter
            .u64_counter("audit_writes_total")
            .with_description(
                "Audit writes to the audit service by write (intent, outcome, rejected, event) and result",
            )
            .build(),
        audit_intent_duration: meter
            .f64_histogram("audit_intent_duration_seconds")
            .with_description(
                "Time an audited request waited on its intent write, retries included",
            )
            .with_boundaries(vec![0.001, 0.005, 0.01, 0.025, 0.05, 0.1, 0.25, 0.5, 1.0])
            .build(),
        audit_queue_depth: meter
            .i64_gauge("audit_queue_depth")
            .with_description(
                "Queued background audit writes waiting to be sent to the audit service",
            )
            .build(),
        config_revision: meter
            .i64_gauge("gateway_config_revision")
            .with_description("The control-plane revision this replica serves")
            .build(),
        config_refused: meter
            .u64_counter("gateway_config_refused_total")
            .with_description("Control-plane snapshots this replica refused, by revision")
            .build(),
        rate_limit_fail_open: meter
            .u64_counter("rate_limit_fail_open_total")
            .with_description(
                "Rate-limit checks allowed fail-open while Valkey was unavailable, by reason",
            )
            .build(),
        valkey_up: meter
            .i64_gauge("valkey_up")
            .with_description(
                "1 when the gateway's Valkey connection is healthy, 0 when failing open",
            )
            .build(),
        admit_invalidations: meter
            .u64_counter("admit_invalidations_total")
            .with_description("Admit cache invalidations received from identity, by scope")
            .build(),
        authorizer_requests: meter
            .u64_counter("authorizer_requests_total")
            .with_description("Outbound AuthZEN PDP requests by authorizer, operation and result")
            .build(),
        authorizer_duration: meter
            .f64_histogram("authorizer_duration_seconds")
            .with_description("Latency of outbound AuthZEN PDP requests")
            .with_boundaries(vec![
                0.001, 0.005, 0.01, 0.025, 0.05, 0.1, 0.25, 0.5, 1.0, 2.5,
            ])
            .build(),
        authorizer_cache: meter
            .u64_counter("authorizer_cache_total")
            .with_description("Outbound AuthZEN decision cache lookups by authorizer and result")
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

static OTEL_ENABLED: AtomicBool = AtomicBool::new(false);

/// Force OTEL instrument creation after MeterProvider is installed.
pub fn init_otel_instruments() {
    LazyLock::force(&OTEL);
    OTEL_ENABLED.store(true, Ordering::Release);
}

#[inline]
fn otel_enabled() -> bool {
    OTEL_ENABLED.load(Ordering::Relaxed)
}

pub fn record_request(route: &str, method: &str, status: u16, duration_secs: f64) {
    let status_s = status.to_string();
    HTTP_REQUESTS_TOTAL
        .with_label_values(&[route, method, &status_s])
        .inc();
    HTTP_REQUEST_DURATION_SECONDS
        .with_label_values(&[route, method])
        .observe(duration_secs);

    if otel_enabled() {
        let attrs = [
            KeyValue::new("route", route.to_string()),
            KeyValue::new("method", method.to_string()),
            KeyValue::new("status", status_s),
        ];
        OTEL.http_requests.add(1, &attrs);
        OTEL.http_duration.record(duration_secs, &attrs[..2]);
    }
}

pub fn record_auth_failure(auth_type: &str, reason: &str) {
    AUTH_FAILURES_TOTAL
        .with_label_values(&[auth_type, reason])
        .inc();
    if otel_enabled() {
        OTEL.auth_failures.add(
            1,
            &[
                KeyValue::new("auth_type", auth_type.to_string()),
                KeyValue::new("reason", reason.to_string()),
            ],
        );
    }
}

pub fn record_auth_validation(auth_type: &str, outcome: &str, duration_secs: f64) {
    AUTH_VALIDATION_DURATION_SECONDS
        .with_label_values(&[auth_type, outcome])
        .observe(duration_secs);
    if otel_enabled() {
        OTEL.auth_validation.record(
            duration_secs,
            &[
                KeyValue::new("auth_type", auth_type.to_string()),
                KeyValue::new("outcome", outcome.to_string()),
            ],
        );
    }
}

pub fn record_auth_cache_hit(auth_type: &str) {
    AUTH_CACHE_HITS_TOTAL.with_label_values(&[auth_type]).inc();
    if otel_enabled() {
        OTEL.auth_cache_hits
            .add(1, &[KeyValue::new("auth_type", auth_type.to_string())]);
    }
}

pub fn record_auth_cache_miss(auth_type: &str) {
    AUTH_CACHE_MISSES_TOTAL
        .with_label_values(&[auth_type])
        .inc();
    if otel_enabled() {
        OTEL.auth_cache_misses
            .add(1, &[KeyValue::new("auth_type", auth_type.to_string())]);
    }
}

/// `write` is `intent`, `outcome`, `rejected`, or `event`. `result`: `ok`, `failed`,
/// `not_found` (outcome with no intent), or `dropped` (background queue full).
pub fn record_audit_write(write: &str, result: &str) {
    AUDIT_WRITES_TOTAL.with_label_values(&[write, result]).inc();
    if otel_enabled() {
        OTEL.audit_writes.add(
            1,
            &[
                KeyValue::new("write", write.to_string()),
                KeyValue::new("result", result.to_string()),
            ],
        );
    }
}

pub fn record_audit_intent_duration(result: &str, duration_secs: f64) {
    AUDIT_INTENT_DURATION_SECONDS
        .with_label_values(&[result])
        .observe(duration_secs);
    if otel_enabled() {
        OTEL.audit_intent_duration.record(
            duration_secs,
            &[KeyValue::new("result", result.to_string())],
        );
    }
}

/// The revision this replica now serves.
pub fn record_config_revision(revision: i64) {
    GATEWAY_CONFIG_REVISION.set(revision);
    if otel_enabled() {
        OTEL.config_revision.record(revision, &[]);
    }
}

/// A snapshot this replica refused.
pub fn record_config_refused(revision: i64) {
    let revision = revision.to_string();
    GATEWAY_CONFIG_REFUSED_TOTAL
        .with_label_values(&[&revision])
        .inc();
    if otel_enabled() {
        OTEL.config_refused
            .add(1, &[KeyValue::new("revision", revision)]);
    }
}

/// A rate-limit check allowed fail-open while Valkey was unavailable.
/// `reason` is `route`, `route_checks`, or `auth_failure`.
pub fn record_rate_limit_fail_open(reason: &str) {
    RATE_LIMIT_FAIL_OPEN_TOTAL
        .with_label_values(&[reason])
        .inc();
    if otel_enabled() {
        OTEL.rate_limit_fail_open
            .add(1, &[KeyValue::new("reason", reason.to_string())]);
    }
}

pub fn record_valkey_up(up: bool) {
    let val = i64::from(up);
    VALKEY_UP.set(val);
    if otel_enabled() {
        OTEL.valkey_up.record(val, &[]);
    }
}

pub fn record_audit_queue_depth(depth: i64) {
    AUDIT_QUEUE_DEPTH.set(depth);
    if otel_enabled() {
        OTEL.audit_queue_depth.record(depth, &[]);
    }
}

/// An admit cache invalidation received from identity (`key`, `user`, `member`, `root`, `reset`, `config_revision`).
pub fn record_admit_invalidation(scope: &str) {
    ADMIT_INVALIDATIONS_TOTAL.with_label_values(&[scope]).inc();
    if otel_enabled() {
        OTEL.admit_invalidations
            .add(1, &[KeyValue::new("scope", scope.to_string())]);
    }
}

pub fn record_authorizer_request(
    authorizer: &str,
    operation: &str,
    result: &str,
    duration_secs: f64,
) {
    AUTHORIZER_REQUESTS_TOTAL
        .with_label_values(&[authorizer, operation, result])
        .inc();
    AUTHORIZER_DURATION_SECONDS
        .with_label_values(&[authorizer, operation])
        .observe(duration_secs);
    if otel_enabled() {
        let attrs = [
            KeyValue::new("authorizer", authorizer.to_string()),
            KeyValue::new("operation", operation.to_string()),
            KeyValue::new("result", result.to_string()),
        ];
        OTEL.authorizer_requests.add(1, &attrs);
        OTEL.authorizer_duration.record(duration_secs, &attrs[..2]);
    }
}

pub fn record_authorizer_cache(authorizer: &str, result: &str) {
    AUTHORIZER_CACHE_TOTAL
        .with_label_values(&[authorizer, result])
        .inc();
    if otel_enabled() {
        OTEL.authorizer_cache.add(
            1,
            &[
                KeyValue::new("authorizer", authorizer.to_string()),
                KeyValue::new("result", result.to_string()),
            ],
        );
    }
}
