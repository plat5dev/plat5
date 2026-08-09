use opentelemetry::global;
use opentelemetry::trace::TracerProvider;
use opentelemetry::KeyValue;
use opentelemetry_otlp::WithExportConfig;
use opentelemetry_sdk::metrics::{PeriodicReader, SdkMeterProvider};
use opentelemetry_sdk::propagation::TraceContextPropagator;
use opentelemetry_sdk::trace as sdktrace;
use opentelemetry_sdk::Resource;
use std::env;
use std::time::Duration;

use tracing_opentelemetry::OpenTelemetryLayer;
use tracing_subscriber::layer::SubscriberExt;
use tracing_subscriber::util::SubscriberInitExt;
use tracing_subscriber::EnvFilter;

use crate::metrics;

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

/// Init tracing + optional OTLP per plat5/docs/telemetry.md.
///
/// Defaults when an OTLP destination is set:
/// - traces → OTLP on (unless OTEL_TRACES_EXPORTER excludes otlp)
/// - metrics OTLP → on when dest exists (OTEL_METRICS_EXPORTER unset defaults to otlp)
/// - /metrics scrape → always on (prometheus; independent of this init)
pub fn init_telemetry() -> Result<TelemetryGuard, Box<dyn std::error::Error + Send + Sync>> {
    let env_filter = EnvFilter::try_from_default_env().unwrap_or_else(|_| EnvFilter::new("info"));
    let json_layer = tracing_subscriber::fmt::layer()
        .json()
        .with_target(false)
        .with_current_span(false)
        .with_span_list(false)
        .flatten_event(true);

    global::set_text_map_propagator(TraceContextPropagator::new());

    let sdk_disabled = env_truthy("OTEL_SDK_DISABLED");
    let traces_endpoint = resolve_otlp_endpoint("OTEL_EXPORTER_OTLP_TRACES_ENDPOINT", "/v1/traces");
    let metrics_endpoint =
        resolve_otlp_endpoint("OTEL_EXPORTER_OTLP_METRICS_ENDPOINT", "/v1/metrics");

    let enable_traces = !sdk_disabled
        && traces_endpoint.is_some()
        && exporter_includes_otlp(env_exporter_list("OTEL_TRACES_EXPORTER"), true);
    let enable_metrics_otlp = !sdk_disabled
        && metrics_endpoint.is_some()
        && exporter_includes_otlp(env_exporter_list("OTEL_METRICS_EXPORTER"), true);

    if !enable_traces && !enable_metrics_otlp {
        tracing_subscriber::registry()
            .with(env_filter)
            .with(json_layer)
            .try_init()?;
        return Ok(TelemetryGuard {
            tracer_provider: None,
            meter_provider: None,
        });
    }

    let service_name = env::var("OTEL_SERVICE_NAME").unwrap_or_else(|_| "gateway".to_string());
    let service_namespace =
        env::var("OTEL_SERVICE_NAMESPACE").unwrap_or_else(|_| "edge".to_string());
    let service_instance_id = env::var("OTEL_SERVICE_INSTANCE_ID")
        .ok()
        .or_else(|| env::var("HOSTNAME").ok())
        .unwrap_or_else(|| format!("{}-{}", service_name, std::process::id()));
    let deployment_env = env::var("OTEL_DEPLOYMENT_ENV")
        .or_else(|_| env::var("DEPLOYMENT_ENV"))
        .unwrap_or_else(|_| "development".to_string());
    let service_version =
        env::var("OTEL_SERVICE_VERSION").unwrap_or_else(|_| env!("CARGO_PKG_VERSION").to_string());
    let sample_ratio: f64 = env::var("OTEL_TRACES_SAMPLER_RATIO")
        .ok()
        .and_then(|value| value.parse().ok())
        .unwrap_or(1.0);

    let resource = Resource::builder()
        .with_attributes([
            KeyValue::new("service.name", service_name.clone()),
            KeyValue::new("service.namespace", service_namespace),
            KeyValue::new("service.instance.id", service_instance_id),
            KeyValue::new("deployment.environment", deployment_env),
            KeyValue::new("service.version", service_version),
        ])
        .build();

    let mut tracer_provider = None;
    if enable_traces {
        let endpoint = traces_endpoint.expect("traces endpoint checked");
        let exporter = opentelemetry_otlp::SpanExporter::builder()
            .with_http()
            .with_endpoint(endpoint)
            .build()?;

        let provider = sdktrace::SdkTracerProvider::builder()
            .with_sampler(sampler_from_ratio(sample_ratio))
            .with_resource(resource.clone())
            .with_batch_exporter(exporter)
            .build();

        global::set_tracer_provider(provider.clone());
        let tracer = provider.tracer(service_name);

        tracing_subscriber::registry()
            .with(env_filter)
            .with(json_layer)
            .with(OpenTelemetryLayer::new(tracer))
            .try_init()?;

        tracer_provider = Some(provider);
    } else {
        tracing_subscriber::registry()
            .with(env_filter)
            .with(json_layer)
            .try_init()?;
    }

    let mut meter_provider = None;
    if enable_metrics_otlp {
        let endpoint = metrics_endpoint.expect("metrics endpoint checked");
        let exporter = opentelemetry_otlp::MetricExporter::builder()
            .with_http()
            .with_endpoint(endpoint)
            .build()?;

        let mut reader = PeriodicReader::builder(exporter);
        if let Some(interval) = metric_export_interval() {
            reader = reader.with_interval(interval);
        }
        let reader = reader.build();

        let provider = SdkMeterProvider::builder()
            .with_reader(reader)
            .with_resource(resource)
            .build();

        global::set_meter_provider(provider.clone());
        metrics::init_otel_instruments();
        meter_provider = Some(provider);
    }

    Ok(TelemetryGuard {
        tracer_provider,
        meter_provider,
    })
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

fn metric_export_interval() -> Option<Duration> {
    let v = env::var("OTEL_METRIC_EXPORT_INTERVAL").ok()?;
    let ms: u64 = v.trim().parse().ok()?;
    if ms == 0 {
        return None;
    }
    Some(Duration::from_millis(ms))
}

fn resolve_otlp_endpoint(specific_env: &str, default_path: &str) -> Option<String> {
    if let Ok(endpoint) = env::var(specific_env) {
        let trimmed = endpoint.trim();
        if !trimmed.is_empty() {
            return Some(trimmed.to_string());
        }
    }

    let base = env::var("OTEL_EXPORTER_OTLP_ENDPOINT").ok()?;
    let base = base.trim();
    if base.is_empty() {
        return None;
    }
    Some(format!("{}{}", base.trim_end_matches('/'), default_path))
}

fn env_exporter_list(key: &str) -> Option<Vec<String>> {
    let raw = env::var(key).ok()?;
    let parts: Vec<String> = raw
        .split(',')
        .map(|s| s.trim().to_ascii_lowercase())
        .filter(|s| !s.is_empty())
        .collect();
    if parts.is_empty() {
        None
    } else {
        Some(parts)
    }
}

/// Traces and metrics: default when unset = true (otlp when dest exists).
fn exporter_includes_otlp(list: Option<Vec<String>>, default_when_unset: bool) -> bool {
    match list {
        None => default_when_unset,
        Some(exporters) => exporters.iter().any(|e| e == "otlp"),
    }
}

fn env_truthy(key: &str) -> bool {
    env::var(key)
        .map(|v| v.eq_ignore_ascii_case("true"))
        .unwrap_or(false)
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn exporter_includes_otlp_defaults() {
        assert!(exporter_includes_otlp(None, true));
        assert!(!exporter_includes_otlp(None, false));
        assert!(exporter_includes_otlp(Some(vec!["otlp".into()]), false));
        assert!(exporter_includes_otlp(
            Some(vec!["otlp".into(), "prometheus".into()]),
            false
        ));
        assert!(!exporter_includes_otlp(
            Some(vec!["prometheus".into()]),
            true
        ));
        assert!(!exporter_includes_otlp(Some(vec!["none".into()]), true));
        assert!(!exporter_includes_otlp(Some(vec![]), true));
    }
}
