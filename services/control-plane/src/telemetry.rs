use opentelemetry::global;
use opentelemetry::trace::TracerProvider;
use opentelemetry::KeyValue;
use opentelemetry_otlp::WithExportConfig;
use opentelemetry_sdk::error::OTelSdkResult;
use opentelemetry_sdk::metrics::data::ResourceMetrics;
use opentelemetry_sdk::metrics::exporter::PushMetricExporter;
use opentelemetry_sdk::metrics::{PeriodicReader, SdkMeterProvider, Temporality};
use opentelemetry_sdk::propagation::TraceContextPropagator;
use opentelemetry_sdk::trace::{self as sdktrace, SpanData, SpanExporter};
use opentelemetry_sdk::Resource;
use std::env;
use std::time::Duration;

use serde_json::Value;
use std::fmt;
use tracing::field::{Field, Visit};
use tracing::{Event, Level, Subscriber};
use tracing_opentelemetry::OpenTelemetryLayer;
use tracing_subscriber::fmt::format::Writer;
use tracing_subscriber::fmt::time::{FormatTime, SystemTime};
use tracing_subscriber::fmt::{FmtContext, FormatEvent, FormatFields, MakeWriter};
use tracing_subscriber::layer::SubscriberExt;
use tracing_subscriber::registry::LookupSpan;
use tracing_subscriber::util::SubscriberInitExt;
use tracing_subscriber::{EnvFilter, Layer, Registry};

use crate::metrics;

const SERVICE_NAME: &str = "control-plane";
const SERVICE_NAMESPACE: &str = "edge";

/// The release this binary was built for: `VERSION` at build time, else `dev`.
pub const VERSION: &str = compiled_version(option_env!("VERSION"));

const fn compiled_version(version: Option<&'static str>) -> &'static str {
    match version {
        Some(v) if !v.is_empty() => v,
        _ => "dev",
    }
}

pub struct TelemetryGuard {
    tracer_provider: Option<sdktrace::SdkTracerProvider>,
    meter_provider: Option<SdkMeterProvider>,
}

impl Drop for TelemetryGuard {
    fn drop(&mut self) {
        if let Some(provider) = self.meter_provider.take() {
            if let Err(err) = provider.shutdown() {
                eprintln!("failed to shut down meter provider: {err:?}");
            }
        }
        if let Some(provider) = self.tracer_provider.take() {
            if let Err(err) = provider.shutdown() {
                eprintln!("failed to shut down tracer provider: {err:?}");
            }
        }
    }
}

/// The JSON log layer: one JSON object per line, on `writer`.
pub(crate) fn json_layer<W>(writer: W) -> impl Layer<Registry> + Send + Sync
where
    W: for<'w> MakeWriter<'w> + Send + Sync + 'static,
{
    tracing_subscriber::fmt::layer()
        .event_format(JsonLine)
        .with_writer(writer)
}

/// A log line as docs/reference/telemetry.md#logs has it: `timestamp`, `level`
/// (`debug`, `info`, `warn` or `error`), `message`, then the event's fields in the
/// order they were written.
struct JsonLine;

impl<S, N> FormatEvent<S, N> for JsonLine
where
    S: Subscriber + for<'a> LookupSpan<'a>,
    N: for<'a> FormatFields<'a> + 'static,
{
    fn format_event(
        &self,
        _ctx: &FmtContext<'_, S, N>,
        mut writer: Writer<'_>,
        event: &Event<'_>,
    ) -> fmt::Result {
        let mut timestamp = String::new();
        SystemTime.format_time(&mut Writer::new(&mut timestamp))?;
        let mut fields = LineFields::default();
        event.record(&mut fields);
        let mut line = vec![
            ("timestamp", Value::String(timestamp)),
            (
                "level",
                Value::String(level(event.metadata().level()).into()),
            ),
        ];
        if let Some(message) = fields.message {
            line.push(("message", Value::String(message)));
        }
        line.extend(fields.rest);
        let mut out = String::from("{");
        for (i, (key, value)) in line.iter().enumerate() {
            if i > 0 {
                out.push(',');
            }
            out.push_str(&Value::String((*key).to_string()).to_string());
            out.push(':');
            out.push_str(&value.to_string());
        }
        out.push('}');
        writeln!(writer, "{out}")
    }
}

/// The contract's four levels. A trace event, below the default filter, is `debug`.
fn level(level: &Level) -> &'static str {
    match *level {
        Level::ERROR => "error",
        Level::WARN => "warn",
        Level::INFO => "info",
        _ => "debug",
    }
}

#[derive(Default)]
struct LineFields {
    message: Option<String>,
    rest: Vec<(&'static str, Value)>,
}

impl LineFields {
    fn put(&mut self, field: &Field, value: Value) {
        if field.name() == "message" {
            self.message = Some(match value {
                Value::String(s) => s,
                other => other.to_string(),
            });
        } else {
            self.rest.push((field.name(), value));
        }
    }
}

impl Visit for LineFields {
    fn record_str(&mut self, field: &Field, value: &str) {
        self.put(field, Value::String(value.to_string()));
    }
    fn record_bool(&mut self, field: &Field, value: bool) {
        self.put(field, Value::Bool(value));
    }
    fn record_i64(&mut self, field: &Field, value: i64) {
        self.put(field, value.into());
    }
    fn record_u64(&mut self, field: &Field, value: u64) {
        self.put(field, value.into());
    }
    fn record_f64(&mut self, field: &Field, value: f64) {
        self.put(
            field,
            serde_json::Number::from_f64(value).map_or(Value::Null, Value::Number),
        );
    }
    fn record_error(&mut self, field: &Field, value: &(dyn std::error::Error + 'static)) {
        self.put(field, Value::String(value.to_string()));
    }
    fn record_debug(&mut self, field: &Field, value: &dyn fmt::Debug) {
        self.put(field, Value::String(format!("{value:?}")));
    }
}

/// Init tracing + optional OTLP per docs/reference/telemetry.md.
///
/// Defaults when an OTLP destination is set:
/// - traces → OTLP on (unless OTEL_TRACES_EXPORTER excludes otlp)
/// - metrics OTLP → on when dest exists (OTEL_METRICS_EXPORTER unset defaults to otlp)
/// - /metrics scrape → always on (prometheus; independent of this init)
///
/// A telemetry variable that is set but invalid is an error naming it, whether or
/// not OTLP is on. A failed export is a JSON error line; the service keeps serving.
pub fn init_telemetry() -> Result<TelemetryGuard, Box<dyn std::error::Error + Send + Sync>> {
    metrics::init();

    let settings = parse_settings(&|key| env::var(key).ok())?;

    let env_filter = EnvFilter::try_from_default_env().unwrap_or_else(|_| EnvFilter::new("info"));
    global::set_text_map_propagator(TraceContextPropagator::new());

    let resource = Resource::builder()
        .with_attributes(settings.resource.clone())
        .build();

    let mut tracer_provider = None;
    let otel_layer = match settings.traces_endpoint {
        Some(ref endpoint) if settings.traces_on() => {
            let exporter = opentelemetry_otlp::SpanExporter::builder()
                .with_http()
                .with_endpoint(endpoint)
                .build()?;
            let provider = sdktrace::SdkTracerProvider::builder()
                .with_sampler(sampler_from_ratio(settings.sampler_ratio))
                .with_resource(resource.clone())
                .with_batch_exporter(LoggedExporter::new("traces", exporter))
                .build();
            global::set_tracer_provider(provider.clone());
            let layer = OpenTelemetryLayer::new(provider.tracer(SERVICE_NAME));
            tracer_provider = Some(provider);
            Some(layer)
        }
        _ => None,
    };

    tracing_subscriber::registry()
        .with(json_layer(std::io::stdout))
        .with(otel_layer)
        .with(env_filter)
        .try_init()?;

    let mut meter_provider = None;
    if let Some(ref endpoint) = settings.metrics_endpoint {
        if settings.metrics_on() {
            let exporter = opentelemetry_otlp::MetricExporter::builder()
                .with_http()
                .with_endpoint(endpoint)
                .build()?;
            let reader = PeriodicReader::builder(LoggedExporter::new("metrics", exporter))
                .with_interval(settings.metric_export_interval)
                .build();
            let provider = SdkMeterProvider::builder()
                .with_reader(reader)
                .with_resource(resource)
                .build();
            global::set_meter_provider(provider.clone());
            metrics::init_otel_instruments();
            meter_provider = Some(provider);
        }
    }

    Ok(TelemetryGuard {
        tracer_provider,
        meter_provider,
    })
}

/// An exporter whose failed exports are logged as one JSON line each. The
/// service keeps running.
#[derive(Debug)]
struct LoggedExporter<E> {
    signal: &'static str,
    inner: E,
}

impl<E> LoggedExporter<E> {
    fn new(signal: &'static str, inner: E) -> Self {
        Self { signal, inner }
    }

    fn log(&self, result: OTelSdkResult) -> OTelSdkResult {
        if let Err(ref err) = result {
            tracing::error!(
                signal = self.signal,
                error_message = %err,
                "telemetry export failed"
            );
        }
        result
    }
}

impl<E: SpanExporter> SpanExporter for LoggedExporter<E> {
    async fn export(&self, batch: Vec<SpanData>) -> OTelSdkResult {
        self.log(self.inner.export(batch).await)
    }

    fn shutdown_with_timeout(&mut self, timeout: Duration) -> OTelSdkResult {
        self.inner.shutdown_with_timeout(timeout)
    }

    fn force_flush(&mut self) -> OTelSdkResult {
        self.inner.force_flush()
    }

    fn set_resource(&mut self, resource: &Resource) {
        self.inner.set_resource(resource)
    }
}

impl<E: PushMetricExporter> PushMetricExporter for LoggedExporter<E> {
    async fn export(&self, metrics: &ResourceMetrics) -> OTelSdkResult {
        self.log(self.inner.export(metrics).await)
    }

    fn force_flush(&self) -> OTelSdkResult {
        self.inner.force_flush()
    }

    fn shutdown_with_timeout(&self, timeout: Duration) -> OTelSdkResult {
        self.inner.shutdown_with_timeout(timeout)
    }

    fn temporality(&self) -> Temporality {
        self.inner.temporality()
    }
}

/// The keys whose values `url.query` never carries (docs/reference/telemetry.md,
/// HTTP server spans). Keys match exactly.
const REDACTED_QUERY_KEYS: [&str; 7] = [
    "sig",
    "Signature",
    "AWSAccessKeyId",
    "X-Amz-Signature",
    "X-Amz-Credential",
    "X-Amz-Security-Token",
    "X-Goog-Signature",
];

/// `url.query`: the raw query with each redacted key's value replaced by
/// `REDACTED`. Every other key and value is kept as sent.
pub fn redact_query(query: &str) -> String {
    query
        .split('&')
        .map(|part| match part.split_once('=') {
            Some((key, _)) if REDACTED_QUERY_KEYS.contains(&key) => format!("{key}=REDACTED"),
            _ => part.to_string(),
        })
        .collect::<Vec<_>>()
        .join("&")
}

/// The telemetry variables, parsed strictly: a variable that is set but invalid
/// is an error naming it (docs/reference/telemetry.md, Invalid values).
#[derive(Debug)]
struct Settings {
    sdk_disabled: bool,
    traces_exporters: Option<Vec<String>>,
    metrics_exporters: Option<Vec<String>>,
    traces_endpoint: Option<String>,
    metrics_endpoint: Option<String>,
    sampler_ratio: f64,
    metric_export_interval: Duration,
    resource: Vec<KeyValue>,
}

impl Settings {
    fn traces_on(&self) -> bool {
        !self.sdk_disabled
            && self.traces_endpoint.is_some()
            && exporter_includes_otlp(self.traces_exporters.as_deref(), true)
    }

    fn metrics_on(&self) -> bool {
        !self.sdk_disabled
            && self.metrics_endpoint.is_some()
            && exporter_includes_otlp(self.metrics_exporters.as_deref(), true)
    }
}

type Lookup<'a> = &'a dyn Fn(&str) -> Option<String>;

/// A variable's value, trimmed. An empty variable is unset.
fn get(env: Lookup, key: &str) -> Option<String> {
    env(key)
        .map(|v| v.trim().to_string())
        .filter(|v| !v.is_empty())
}

fn parse_settings(env: Lookup) -> Result<Settings, String> {
    let sdk_disabled = match get(env, "OTEL_SDK_DISABLED") {
        None => false,
        Some(v) if v.eq_ignore_ascii_case("false") => false,
        Some(v) if v.eq_ignore_ascii_case("true") => true,
        Some(v) => return Err(format!("OTEL_SDK_DISABLED is {v:?}: want true or false")),
    };
    for key in [
        "OTEL_EXPORTER_OTLP_PROTOCOL",
        "OTEL_EXPORTER_OTLP_TRACES_PROTOCOL",
        "OTEL_EXPORTER_OTLP_METRICS_PROTOCOL",
    ] {
        if let Some(v) = get(env, key) {
            if v != "http/protobuf" {
                return Err(format!("{key} is {v:?}: only http/protobuf is supported"));
            }
        }
    }
    let traces_exporters = exporter_list(env, "OTEL_TRACES_EXPORTER", &["otlp", "none"])?;
    let metrics_exporters = exporter_list(
        env,
        "OTEL_METRICS_EXPORTER",
        &["otlp", "prometheus", "none"],
    )?;
    let sampler_ratio = match get(env, "OTEL_TRACES_SAMPLER_RATIO") {
        None => 1.0,
        Some(v) => v
            .parse::<f64>()
            .ok()
            .filter(|r| (0.0..=1.0).contains(r))
            .ok_or_else(|| {
                format!("OTEL_TRACES_SAMPLER_RATIO is {v:?}: want a number from 0 to 1")
            })?,
    };
    let metric_export_interval = match get(env, "OTEL_METRIC_EXPORT_INTERVAL") {
        None => Duration::from_secs(60),
        Some(v) => v
            .parse::<u64>()
            .ok()
            .filter(|ms| *ms > 0)
            .map(Duration::from_millis)
            .ok_or_else(|| {
                format!(
                    "OTEL_METRIC_EXPORT_INTERVAL is {v:?}: want a positive number of milliseconds"
                )
            })?,
    };
    let base = endpoint(env, "OTEL_EXPORTER_OTLP_ENDPOINT")?;
    let signal_endpoint = |key: &str, path: &str| -> Result<Option<String>, String> {
        Ok(endpoint(env, key)?.or_else(|| {
            base.as_ref()
                .map(|b| format!("{}{}", b.trim_end_matches('/'), path))
        }))
    };
    let traces_endpoint = signal_endpoint("OTEL_EXPORTER_OTLP_TRACES_ENDPOINT", "/v1/traces")?;
    let metrics_endpoint = signal_endpoint("OTEL_EXPORTER_OTLP_METRICS_ENDPOINT", "/v1/metrics")?;
    let resource = resource_attributes(env, VERSION)?;

    Ok(Settings {
        sdk_disabled,
        traces_exporters,
        metrics_exporters,
        traces_endpoint,
        metrics_endpoint,
        sampler_ratio,
        metric_export_interval,
        resource,
    })
}

/// An OTLP endpoint variable: unset, or an `http://` or `https://` URL.
fn endpoint(env: Lookup, key: &str) -> Result<Option<String>, String> {
    let Some(v) = get(env, key) else {
        return Ok(None);
    };
    let ok = v
        .parse::<http::Uri>()
        .ok()
        .filter(|u| matches!(u.scheme_str(), Some("http" | "https")))
        .is_some_and(|u| u.authority().is_some_and(|a| !a.host().is_empty()));
    if !ok {
        return Err(format!("{key} is {v:?}: want an http:// or https:// URL"));
    }
    Ok(Some(v))
}

/// A comma-separated `OTEL_*_EXPORTER` list. `None` when unset (callers apply
/// defaults); a value outside `allowed` is an error.
fn exporter_list(env: Lookup, key: &str, allowed: &[&str]) -> Result<Option<Vec<String>>, String> {
    let Some(raw) = get(env, key) else {
        return Ok(None);
    };
    let mut out = Vec::new();
    for part in raw.split(',').map(|s| s.trim().to_ascii_lowercase()) {
        if part.is_empty() {
            continue;
        }
        if !allowed.contains(&part.as_str()) {
            return Err(format!(
                "{key} is {raw:?}: want a comma-separated list of {}",
                allowed.join(", ")
            ));
        }
        out.push(part);
    }
    Ok(if out.is_empty() { None } else { Some(out) })
}

/// The resource, layered: the service's defaults (`version` among them), then
/// `OTEL_RESOURCE_ATTRIBUTES`, then `OTEL_SERVICE_NAME`, then the convenience
/// variables that are set.
fn resource_attributes(env: Lookup, version: &str) -> Result<Vec<KeyValue>, String> {
    let mut attrs: Vec<(String, String)> = vec![
        ("service.name".into(), SERVICE_NAME.into()),
        ("service.namespace".into(), SERVICE_NAMESPACE.into()),
        (
            "service.instance.id".into(),
            get(env, "HOSTNAME")
                .unwrap_or_else(|| format!("{SERVICE_NAME}-{}", std::process::id())),
        ),
        ("service.version".into(), version.into()),
        ("deployment.environment".into(), "development".into()),
    ];
    let mut set = |key: &str, value: String| {
        attrs.retain(|(k, _)| k != key);
        attrs.push((key.to_string(), value));
    };
    if let Some(raw) = get(env, "OTEL_RESOURCE_ATTRIBUTES") {
        for entry in raw.split(',') {
            let Some((key, value)) = entry.split_once('=').filter(|(k, _)| !k.trim().is_empty())
            else {
                let want = "want key=value entries, comma-separated";
                return Err(format!("OTEL_RESOURCE_ATTRIBUTES is {raw:?}: {want}"));
            };
            set(key.trim(), value.trim().to_string());
        }
    }
    for (key, vars) in [
        ("service.name", &["OTEL_SERVICE_NAME"][..]),
        ("service.namespace", &["OTEL_SERVICE_NAMESPACE"]),
        ("service.version", &["OTEL_SERVICE_VERSION"]),
        ("service.instance.id", &["OTEL_SERVICE_INSTANCE_ID"]),
        (
            "deployment.environment",
            &["OTEL_DEPLOYMENT_ENV", "DEPLOYMENT_ENV"],
        ),
    ] {
        if let Some(value) = vars.iter().find_map(|v| get(env, v)) {
            set(key, value);
        }
    }
    Ok(attrs
        .into_iter()
        .map(|(k, v)| KeyValue::new(k, v))
        .collect())
}

fn sampler_from_ratio(ratio: f64) -> sdktrace::Sampler {
    let inner = if (ratio - 1.0).abs() < f64::EPSILON {
        sdktrace::Sampler::AlwaysOn
    } else if ratio <= 0.0 {
        sdktrace::Sampler::AlwaysOff
    } else {
        sdktrace::Sampler::TraceIdRatioBased(ratio)
    };
    sdktrace::Sampler::ParentBased(Box::new(inner))
}

/// Traces and metrics: default when unset = true (otlp when dest exists).
fn exporter_includes_otlp(list: Option<&[String]>, default_when_unset: bool) -> bool {
    match list {
        None => default_when_unset,
        Some(exporters) => exporters.iter().any(|e| e == "otlp"),
    }
}

#[cfg(test)]
pub(crate) mod tests {
    use super::*;
    use std::collections::HashMap;
    use std::sync::{Arc, Mutex};

    fn lookup(vars: &[(&str, &str)]) -> impl Fn(&str) -> Option<String> {
        let vars: HashMap<String, String> = vars
            .iter()
            .map(|(k, v)| (k.to_string(), v.to_string()))
            .collect();
        move |key| vars.get(key).cloned()
    }

    fn settings(vars: &[(&str, &str)]) -> Result<Settings, String> {
        parse_settings(&lookup(vars))
    }

    #[test]
    fn exporter_includes_otlp_defaults() {
        let list = |items: &[&str]| items.iter().map(|s| s.to_string()).collect::<Vec<_>>();
        assert!(exporter_includes_otlp(None, true));
        assert!(!exporter_includes_otlp(None, false));
        assert!(exporter_includes_otlp(Some(&list(&["otlp"])), false));
        assert!(exporter_includes_otlp(
            Some(&list(&["otlp", "prometheus"])),
            false
        ));
        assert!(!exporter_includes_otlp(Some(&list(&["prometheus"])), true));
        assert!(!exporter_includes_otlp(Some(&list(&["none"])), true));
        assert!(!exporter_includes_otlp(Some(&[]), true));
    }

    /// Each set-but-invalid value in telemetry.md's "Invalid values" table stops
    /// boot with an error naming the variable.
    #[test]
    fn invalid_values_are_refused_naming_the_variable() {
        for (key, value) in [
            ("OTEL_TRACES_EXPORTER", "jaeger"),
            ("OTEL_TRACES_EXPORTER", "otlp,prometheus"),
            ("OTEL_METRICS_EXPORTER", "otlp,statsd"),
            ("OTEL_EXPORTER_OTLP_PROTOCOL", "grpc"),
            ("OTEL_EXPORTER_OTLP_TRACES_PROTOCOL", "http/json"),
            ("OTEL_EXPORTER_OTLP_METRICS_PROTOCOL", "grpc"),
            ("OTEL_EXPORTER_OTLP_ENDPOINT", "localhost:4318"),
            ("OTEL_EXPORTER_OTLP_ENDPOINT", "grpc://collector:4317"),
            (
                "OTEL_EXPORTER_OTLP_TRACES_ENDPOINT",
                "ftp://collector/v1/traces",
            ),
            ("OTEL_EXPORTER_OTLP_METRICS_ENDPOINT", "collector"),
            ("OTEL_TRACES_SAMPLER_RATIO", "1.5"),
            ("OTEL_TRACES_SAMPLER_RATIO", "-0.1"),
            ("OTEL_TRACES_SAMPLER_RATIO", "half"),
            ("OTEL_METRIC_EXPORT_INTERVAL", "0"),
            ("OTEL_METRIC_EXPORT_INTERVAL", "10s"),
            ("OTEL_SDK_DISABLED", "yes"),
            ("OTEL_RESOURCE_ATTRIBUTES", "service.namespace"),
            ("OTEL_RESOURCE_ATTRIBUTES", "a=b,=c"),
        ] {
            let err = settings(&[(key, value)]).expect_err(&format!("accepted {key}={value}"));
            assert!(err.contains(key), "{err:?} does not name {key}");
        }
    }

    #[test]
    fn valid_values_are_accepted() {
        let s = settings(&[
            ("OTEL_TRACES_EXPORTER", "otlp, none"),
            ("OTEL_METRICS_EXPORTER", "Prometheus"),
            ("OTEL_EXPORTER_OTLP_PROTOCOL", "http/protobuf"),
            ("OTEL_EXPORTER_OTLP_ENDPOINT", "https://collector.example/"),
            ("OTEL_TRACES_SAMPLER_RATIO", "0.25"),
            ("OTEL_METRIC_EXPORT_INTERVAL", "5000"),
            ("OTEL_SDK_DISABLED", "false"),
            (
                "OTEL_RESOURCE_ATTRIBUTES",
                "service.namespace=x,deployment.environment=dev",
            ),
            ("OTEL_EXPORTER_OTLP_METRICS_ENDPOINT", " "),
        ])
        .unwrap();
        assert!(s.traces_on());
        assert!(!s.metrics_on());
        assert_eq!(
            s.traces_endpoint.as_deref(),
            Some("https://collector.example/v1/traces")
        );
        assert_eq!(s.metric_export_interval, Duration::from_millis(5000));
        assert_eq!(s.sampler_ratio, 0.25);
        assert!(!settings(&[]).unwrap().traces_on());
    }

    fn version(vars: &[(&str, &str)], compiled: &str) -> String {
        resource_attributes(&lookup(vars), compiled)
            .unwrap()
            .into_iter()
            .find(|kv| kv.key.as_str() == "service.version")
            .unwrap()
            .value
            .to_string()
    }

    /// `service.version`: `OTEL_SERVICE_VERSION`, then `OTEL_RESOURCE_ATTRIBUTES`,
    /// then the compiled `VERSION`, which is `dev` without one.
    #[test]
    fn service_version_is_the_build_unless_overridden() {
        assert_eq!(compiled_version(None), "dev");
        assert_eq!(compiled_version(Some("")), "dev");
        assert_eq!(compiled_version(Some("v1.2.3")), "v1.2.3");
        assert_eq!(VERSION, compiled_version(option_env!("VERSION")));

        assert_eq!(version(&[("CI_COMMIT_TAG", "v9.9.9")], "dev"), "dev");
        assert_eq!(version(&[], "v1.2.3"), "v1.2.3");
        let attrs = ("OTEL_RESOURCE_ATTRIBUTES", "service.version=v2.0.0");
        assert_eq!(version(&[attrs], "v1.2.3"), "v2.0.0");
        assert_eq!(
            version(&[attrs, ("OTEL_SERVICE_VERSION", "v3.0.0")], "v1.2.3"),
            "v3.0.0"
        );
    }

    #[test]
    fn redacts_the_standard_keys_only() {
        assert_eq!(redact_query("a=1&b=2"), "a=1&b=2");
        assert_eq!(redact_query("sig=abc&a=1"), "sig=REDACTED&a=1");
        assert_eq!(
            redact_query("X-Amz-Signature=x&X-Amz-Credential=y&X-Amz-Security-Token=z"),
            "X-Amz-Signature=REDACTED&X-Amz-Credential=REDACTED&X-Amz-Security-Token=REDACTED"
        );
        assert_eq!(
            redact_query("Signature=s&AWSAccessKeyId=k&X-Goog-Signature=g"),
            "Signature=REDACTED&AWSAccessKeyId=REDACTED&X-Goog-Signature=REDACTED"
        );
        assert_eq!(redact_query("SIG=abc&sig"), "SIG=abc&sig");
        assert_eq!(redact_query("q=a%20b+c&sig="), "q=a%20b+c&sig=REDACTED");
    }

    #[derive(Clone, Default)]
    pub(crate) struct Buffer(pub(crate) Arc<Mutex<Vec<u8>>>);

    impl std::io::Write for Buffer {
        fn write(&mut self, buf: &[u8]) -> std::io::Result<usize> {
            self.0.lock().unwrap().extend_from_slice(buf);
            Ok(buf.len())
        }
        fn flush(&mut self) -> std::io::Result<()> {
            Ok(())
        }
    }

    impl<'a> MakeWriter<'a> for Buffer {
        type Writer = Buffer;
        fn make_writer(&'a self) -> Self::Writer {
            self.clone()
        }
    }

    #[derive(Debug)]
    struct Unreachable;

    impl SpanExporter for Unreachable {
        async fn export(&self, _batch: Vec<SpanData>) -> OTelSdkResult {
            Err(opentelemetry_sdk::error::OTelSdkError::InternalFailure(
                "connection refused".into(),
            ))
        }
    }

    /// Each line is one JSON object: `timestamp`, the level in lowercase, `message`,
    /// then the fields as written (docs/reference/telemetry.md#logs).
    #[test]
    fn lines_have_the_contract_fields_and_lowercase_levels() {
        let buf = Buffer::default();
        let subscriber = tracing_subscriber::registry().with(json_layer(buf.clone()));
        tracing::subscriber::with_default(subscriber, || {
            tracing::error!(status = 503u16, route = %"/orgs/{organization}", "e");
            tracing::warn!(error = %"refused", "w");
            tracing::info!(duration_ms = 1.5, absent = None::<&str>, "i");
            tracing::debug!("d");
        });
        let out = String::from_utf8(buf.0.lock().unwrap().clone()).unwrap();
        let lines: Vec<&str> = out.lines().collect();
        assert_eq!(lines.len(), 4, "{out}");
        let parsed: Vec<serde_json::Value> = lines
            .iter()
            .map(|l| serde_json::from_str(l).unwrap())
            .collect();
        let levels: Vec<&str> = parsed
            .iter()
            .map(|l| l["level"].as_str().unwrap())
            .collect();
        assert_eq!(levels, ["error", "warn", "info", "debug"]);
        assert!(lines[0].starts_with(r#"{"timestamp":""#), "{}", lines[0]);
        assert!(
            lines[0].contains(
                r#""level":"error","message":"e","status":503,"route":"/orgs/{organization}"}"#
            ),
            "{}",
            lines[0]
        );
        let ts = parsed[0]["timestamp"].as_str().unwrap();
        assert!(ts.ends_with('Z') && ts.contains('T'), "{ts}");
        assert_eq!(parsed[1]["error"], "refused");
        assert_eq!(parsed[2]["duration_ms"], 1.5);
        assert!(parsed[2].get("absent").is_none(), "{}", lines[2]);
    }

    /// A failed export is one JSON line on the service's log.
    #[tokio::test]
    async fn failed_export_is_a_json_line() {
        let buf = Buffer::default();
        let subscriber = tracing_subscriber::registry().with(json_layer(buf.clone()));
        let _guard = tracing::subscriber::set_default(subscriber);

        let exporter = LoggedExporter::new("traces", Unreachable);
        assert!(exporter.export(vec![]).await.is_err());

        let out = String::from_utf8(buf.0.lock().unwrap().clone()).unwrap();
        let lines: Vec<&str> = out.lines().collect();
        assert_eq!(lines.len(), 1, "{out}");
        let line: serde_json::Value = serde_json::from_str(lines[0]).unwrap();
        assert_eq!(line["level"], "error");
        assert_eq!(line["message"], "telemetry export failed");
        assert_eq!(line["signal"], "traces");
        assert_eq!(
            line["error_message"],
            "Operation failed: connection refused"
        );
    }
}
