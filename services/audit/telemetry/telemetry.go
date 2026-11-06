package telemetry

import (
	"context"
	"fmt"
	"net/url"
	"os"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/rs/zerolog"
	"go.opentelemetry.io/contrib/bridges/prometheus"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/exporters/otlp/otlpmetric/otlpmetrichttp"
	"go.opentelemetry.io/otel/exporters/otlp/otlptrace/otlptracehttp"
	"go.opentelemetry.io/otel/propagation"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/resource"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	semconv "go.opentelemetry.io/otel/semconv/v1.24.0"
	"go.opentelemetry.io/otel/trace"
	tracenoop "go.opentelemetry.io/otel/trace/noop"
)

// Version is the release this binary was built for, set at build time with
// -ldflags "-X github.com/plat5dev/plat5/audit/telemetry.Version=$VERSION".
// A build without one reports dev.
var Version = "dev"

// serviceName is the default service.name and service.namespace.
const serviceName = "audit"

// Telemetry bundles tracing, optional OTLP metrics bridge, and logging utilities.
// Prometheus scrape (/metrics) lives in the metrics package and is always on.
type Telemetry struct {
	tracerProvider trace.TracerProvider
	sdkProvider    *sdktrace.TracerProvider
	meterProvider  *sdkmetric.MeterProvider
	propagator     propagation.TextMapPropagator
	logger         zerolog.Logger
}

// Init configures OpenTelemetry per plat5/docs/reference/telemetry.md.
//
// Defaults when an OTLP destination is set:
//   - traces → OTLP on (unless OTEL_TRACES_EXPORTER excludes otlp)
//   - metrics OTLP → on when dest exists (OTEL_METRICS_EXPORTER unset defaults to otlp)
//   - /metrics scrape → always on (metrics package; independent of this Init)
//
// A telemetry variable that is set but invalid is an error naming it, whether or
// not OTLP is on. A failed export is a JSON error line; the service keeps serving.
func Init(ctx context.Context) (*Telemetry, error) {
	zerolog.TimeFieldFormat = time.RFC3339Nano
	zerolog.MessageFieldName = "message"
	zerolog.LevelFieldName = "level"
	zerolog.TimestampFieldName = "timestamp"

	baseLogger := zerolog.New(os.Stdout).
		With().
		Timestamp().
		Logger()

	propagator := propagation.NewCompositeTextMapPropagator(
		propagation.TraceContext{},
	)
	otel.SetTextMapPropagator(propagator)
	otel.SetErrorHandler(ErrorHandler(baseLogger))

	t := &Telemetry{
		propagator: propagator,
		logger:     baseLogger,
	}

	settings, err := parseSettings()
	if err != nil {
		return nil, err
	}

	res, err := buildResource(ctx)
	if err != nil {
		return nil, err
	}

	tracesDest, metricsDest := settings.tracesDest, settings.metricsDest
	enableTraces := !settings.sdkDisabled && tracesDest != nil && exporterIncludesOTLP(settings.tracesExporters, true)
	enableMetricsOTLP := !settings.sdkDisabled && metricsDest != nil && exporterIncludesOTLP(settings.metricsExporters, true)

	if !enableTraces && !enableMetricsOTLP {
		tp := tracenoop.NewTracerProvider()
		otel.SetTracerProvider(tp)
		t.tracerProvider = tp
		return t, nil
	}

	if enableTraces {
		clientOpts := []otlptracehttp.Option{
			otlptracehttp.WithEndpoint(tracesDest.host),
			otlptracehttp.WithURLPath(tracesDest.path),
		}
		if tracesDest.insecure {
			clientOpts = append(clientOpts, otlptracehttp.WithInsecure())
		}
		exporter, err := otlptracehttp.New(ctx, clientOpts...)
		if err != nil {
			return nil, fmt.Errorf("failed to create trace exporter: %w", err)
		}
		tp := sdktrace.NewTracerProvider(
			sdktrace.WithSampler(sdktrace.ParentBased(sdktrace.TraceIDRatioBased(settings.samplerRatio))),
			sdktrace.WithBatcher(exporter),
			sdktrace.WithResource(res),
		)
		otel.SetTracerProvider(tp)
		t.tracerProvider = tp
		t.sdkProvider = tp
	} else {
		tp := tracenoop.NewTracerProvider()
		otel.SetTracerProvider(tp)
		t.tracerProvider = tp
	}

	if enableMetricsOTLP {
		metricOpts := []otlpmetrichttp.Option{
			otlpmetrichttp.WithEndpoint(metricsDest.host),
			otlpmetrichttp.WithURLPath(metricsDest.path),
		}
		if metricsDest.insecure {
			metricOpts = append(metricOpts, otlpmetrichttp.WithInsecure())
		}
		metricExporter, err := otlpmetrichttp.New(ctx, metricOpts...)
		if err != nil {
			return nil, fmt.Errorf("failed to create metric exporter: %w", err)
		}
		reader := sdkmetric.NewPeriodicReader(metricExporter,
			sdkmetric.WithProducer(prometheus.NewMetricProducer()),
			sdkmetric.WithInterval(settings.metricExportInterval),
		)
		mp := sdkmetric.NewMeterProvider(
			sdkmetric.WithResource(res),
			sdkmetric.WithReader(reader),
		)
		otel.SetMeterProvider(mp)
		t.meterProvider = mp
	}

	return t, nil
}

// buildResource layers the resource: the service's defaults (the compiled
// Version among them), then OTEL_RESOURCE_ATTRIBUTES and OTEL_SERVICE_NAME,
// then the convenience variables that are set.
func buildResource(ctx context.Context) (*resource.Resource, error) {
	defaults := []attribute.KeyValue{
		semconv.ServiceNameKey.String(serviceName),
		semconv.ServiceNamespaceKey.String(serviceName),
		semconv.ServiceInstanceIDKey.String(getEnv("HOSTNAME", serviceName+"-local")),
		semconv.ServiceVersionKey.String(Version),
		semconv.DeploymentEnvironmentKey.String("development"),
	}
	var overrides []attribute.KeyValue
	for _, o := range []struct {
		key  attribute.Key
		vars []string
	}{
		{semconv.ServiceNamespaceKey, []string{"OTEL_SERVICE_NAMESPACE"}},
		{semconv.ServiceVersionKey, []string{"OTEL_SERVICE_VERSION"}},
		{semconv.ServiceInstanceIDKey, []string{"OTEL_SERVICE_INSTANCE_ID"}},
		{semconv.DeploymentEnvironmentKey, []string{"OTEL_DEPLOYMENT_ENV", "DEPLOYMENT_ENV"}},
	} {
		for _, v := range o.vars {
			if value := getEnv(v, ""); value != "" {
				overrides = append(overrides, o.key.String(value))
				break
			}
		}
	}

	res, err := resource.New(ctx,
		resource.WithAttributes(defaults...),
		resource.WithFromEnv(),
		resource.WithProcess(),
		resource.WithTelemetrySDK(),
		resource.WithAttributes(overrides...),
	)
	if err != nil {
		return nil, fmt.Errorf("failed to build resource: %w", err)
	}
	return res, nil
}

// ErrorHandler logs an OpenTelemetry error (a failed export, say) as one JSON
// line. The service keeps running.
func ErrorHandler(logger zerolog.Logger) otel.ErrorHandler {
	return otel.ErrorHandlerFunc(func(err error) {
		logger.Error().Str("error_message", err.Error()).Msg("telemetry error")
	})
}

// Shutdown flushes exporters when OTLP is enabled.
func (t *Telemetry) Shutdown(ctx context.Context) error {
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	var firstErr error
	if t.meterProvider != nil {
		if err := t.meterProvider.Shutdown(ctx); err != nil && firstErr == nil {
			firstErr = err
		}
	}
	if t.sdkProvider != nil {
		if err := t.sdkProvider.Shutdown(ctx); err != nil && firstErr == nil {
			firstErr = err
		}
	}
	return firstErr
}

// TracerProvider returns the underlying provider.
func (t *Telemetry) TracerProvider() trace.TracerProvider {
	return t.tracerProvider
}

// Propagator returns the configured propagator.
func (t *Telemetry) Propagator() propagation.TextMapPropagator {
	return t.propagator
}

// Logger returns the base zerolog logger.
func (t *Telemetry) Logger() zerolog.Logger {
	return t.logger
}

// LoggerWithContext enriches the logger with trace/span IDs if available.
func (t *Telemetry) LoggerWithContext(ctx context.Context) zerolog.Logger {
	spanContext := trace.SpanContextFromContext(ctx)
	if !spanContext.IsValid() {
		return t.logger
	}
	return t.logger.With().
		Str("trace_id", spanContext.TraceID().String()).
		Str("span_id", spanContext.SpanID().String()).
		Logger()
}

// settings are the telemetry variables, parsed strictly: a variable that is
// set but invalid is an error naming it (docs/reference/telemetry.md, Invalid values).
type settings struct {
	sdkDisabled          bool
	tracesExporters      []string
	metricsExporters     []string
	tracesDest           *otlpDest
	metricsDest          *otlpDest
	samplerRatio         float64
	metricExportInterval time.Duration
}

func parseSettings() (settings, error) {
	var s settings
	var err error
	if s.sdkDisabled, err = envBool("OTEL_SDK_DISABLED"); err != nil {
		return s, err
	}
	if err = checkProtocol(); err != nil {
		return s, err
	}
	if s.tracesExporters, err = envExporterList("OTEL_TRACES_EXPORTER", "otlp", "none"); err != nil {
		return s, err
	}
	if s.metricsExporters, err = envExporterList("OTEL_METRICS_EXPORTER", "otlp", "prometheus", "none"); err != nil {
		return s, err
	}
	if s.samplerRatio, err = samplerRatio(); err != nil {
		return s, err
	}
	if s.metricExportInterval, err = metricExportInterval(); err != nil {
		return s, err
	}
	if err = checkResourceAttributes(); err != nil {
		return s, err
	}
	if _, err = parseEndpoint("OTEL_EXPORTER_OTLP_ENDPOINT"); err != nil {
		return s, err
	}
	if s.tracesDest, err = resolveOTLPEndpoint("traces", "/v1/traces"); err != nil {
		return s, err
	}
	if s.metricsDest, err = resolveOTLPEndpoint("metrics", "/v1/metrics"); err != nil {
		return s, err
	}
	return s, nil
}

type otlpDest struct {
	host     string
	path     string
	insecure bool
}

// parseEndpoint is an OTLP endpoint variable: nil when unset, else an http:// or https:// URL.
func parseEndpoint(key string) (*url.URL, error) {
	v := strings.TrimSpace(os.Getenv(key))
	if v == "" {
		return nil, nil
	}
	u, err := url.Parse(v)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
		return nil, fmt.Errorf("%s is %q: want an http:// or https:// URL", key, v)
	}
	return u, nil
}

// resolveOTLPEndpoint resolves OTEL_EXPORTER_OTLP_{SIGNAL}_ENDPOINT, else the base
// OTEL_EXPORTER_OTLP_ENDPOINT plus the signal's path. Does not apply SDK_DISABLED or
// exporter lists: callers gate those.
func resolveOTLPEndpoint(signal, defaultPath string) (*otlpDest, error) {
	u, err := parseEndpoint("OTEL_EXPORTER_OTLP_" + strings.ToUpper(signal) + "_ENDPOINT")
	if err != nil {
		return nil, err
	}
	path := ""
	if u != nil {
		// A per-signal URL without a path gets the signal's.
		path = u.Path
	} else {
		if u, err = parseEndpoint("OTEL_EXPORTER_OTLP_ENDPOINT"); err != nil || u == nil {
			return nil, err
		}
		path = strings.TrimSuffix(u.Path, "/") + defaultPath
	}
	if path == "" || path == "/" {
		path = defaultPath
	}
	return &otlpDest{
		host:     u.Host,
		path:     path,
		insecure: u.Scheme != "https",
	}, nil
}

// envExporterList parses a comma-separated OTEL_*_EXPORTER value.
// Returns nil when unset (caller applies defaults); a value outside allowed is an error.
func envExporterList(key string, allowed ...string) ([]string, error) {
	raw := strings.TrimSpace(os.Getenv(key))
	if raw == "" {
		return nil, nil
	}
	parts := strings.Split(raw, ",")
	out := make([]string, 0, len(parts))
	for _, p := range parts {
		p = strings.ToLower(strings.TrimSpace(p))
		if p == "" {
			continue
		}
		if !slices.Contains(allowed, p) {
			return nil, fmt.Errorf("%s is %q: want a comma-separated list of %s", key, raw, strings.Join(allowed, ", "))
		}
		out = append(out, p)
	}
	if len(out) == 0 {
		return nil, nil
	}
	return out, nil
}

// exporterIncludesOTLP reports whether OTLP export is allowed.
// defaultWhenUnset: traces and metrics default true (otlp when dest exists).
func exporterIncludesOTLP(list []string, defaultWhenUnset bool) bool {
	if list == nil {
		return defaultWhenUnset
	}
	return slices.Contains(list, "otlp")
}

// envBool is a boolean OTEL_* variable: unset or empty is false, else true or false.
func envBool(key string) (bool, error) {
	switch v := strings.ToLower(strings.TrimSpace(os.Getenv(key))); v {
	case "", "false":
		return false, nil
	case "true":
		return true, nil
	default:
		return false, fmt.Errorf("%s is %q: want true or false", key, os.Getenv(key))
	}
}

// checkProtocol refuses any OTLP protocol but http/protobuf, the one the exporters speak.
func checkProtocol() error {
	for _, key := range []string{"OTEL_EXPORTER_OTLP_PROTOCOL", "OTEL_EXPORTER_OTLP_TRACES_PROTOCOL", "OTEL_EXPORTER_OTLP_METRICS_PROTOCOL"} {
		if v := strings.TrimSpace(os.Getenv(key)); v != "" && v != "http/protobuf" {
			return fmt.Errorf("%s is %q: only http/protobuf is supported", key, v)
		}
	}
	return nil
}

// checkResourceAttributes refuses OTEL_RESOURCE_ATTRIBUTES unless every entry is key=value.
func checkResourceAttributes() error {
	raw := strings.TrimSpace(os.Getenv("OTEL_RESOURCE_ATTRIBUTES"))
	if raw == "" {
		return nil
	}
	for _, entry := range strings.Split(raw, ",") {
		key, _, ok := strings.Cut(entry, "=")
		if !ok || strings.TrimSpace(key) == "" {
			return fmt.Errorf("OTEL_RESOURCE_ATTRIBUTES is %q: want key=value entries, comma-separated", raw)
		}
	}
	return nil
}

func getEnv(key, fallback string) string {
	if value := strings.TrimSpace(os.Getenv(key)); value != "" {
		return value
	}
	return fallback
}

// samplerRatio is OTEL_TRACES_SAMPLER_RATIO: unset is 1, else a number from 0 to 1.
func samplerRatio() (float64, error) {
	v := strings.TrimSpace(os.Getenv("OTEL_TRACES_SAMPLER_RATIO"))
	if v == "" {
		return 1, nil
	}
	f, err := strconv.ParseFloat(v, 64)
	if err != nil || f < 0 || f > 1 {
		return 0, fmt.Errorf("OTEL_TRACES_SAMPLER_RATIO is %q: want a number from 0 to 1", v)
	}
	return f, nil
}

// metricExportInterval is OTEL_METRIC_EXPORT_INTERVAL, milliseconds per the OTEL env spec:
// unset is 60000, else a positive whole number.
func metricExportInterval() (time.Duration, error) {
	v := strings.TrimSpace(os.Getenv("OTEL_METRIC_EXPORT_INTERVAL"))
	if v == "" {
		return 60 * time.Second, nil
	}
	ms, err := strconv.Atoi(v)
	if err != nil || ms <= 0 {
		return 0, fmt.Errorf("OTEL_METRIC_EXPORT_INTERVAL is %q: want a positive number of milliseconds", v)
	}
	return time.Duration(ms) * time.Millisecond, nil
}
