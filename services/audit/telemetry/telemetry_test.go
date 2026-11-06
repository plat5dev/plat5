package telemetry

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/rs/zerolog"
	"go.opentelemetry.io/otel"
	semconv "go.opentelemetry.io/otel/semconv/v1.24.0"
)

func TestExporterIncludesOTLP(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name             string
		list             []string
		defaultWhenUnset bool
		want             bool
	}{
		{name: "traces default unset", list: nil, defaultWhenUnset: true, want: true},
		{name: "metrics default unset", list: nil, defaultWhenUnset: false, want: false},
		{name: "explicit otlp", list: []string{"otlp"}, defaultWhenUnset: false, want: true},
		{name: "otlp and prometheus", list: []string{"otlp", "prometheus"}, defaultWhenUnset: false, want: true},
		{name: "prometheus only", list: []string{"prometheus"}, defaultWhenUnset: true, want: false},
		{name: "none", list: []string{"none"}, defaultWhenUnset: true, want: false},
		{name: "empty slice not unset", list: []string{}, defaultWhenUnset: true, want: false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			got := exporterIncludesOTLP(tt.list, tt.defaultWhenUnset)
			if got != tt.want {
				t.Fatalf("exporterIncludesOTLP(%v, %v) = %v, want %v", tt.list, tt.defaultWhenUnset, got, tt.want)
			}
		})
	}
}

func TestEnvExporterList(t *testing.T) {
	t.Setenv("OTEL_METRICS_EXPORTER", " otlp , Prometheus ")
	got, err := envExporterList("OTEL_METRICS_EXPORTER", "otlp", "prometheus", "none")
	if err != nil || len(got) != 2 || got[0] != "otlp" || got[1] != "prometheus" {
		t.Fatalf("envExporterList = %#v, %v", got, err)
	}

	t.Setenv("OTEL_METRICS_EXPORTER", "")
	if got, err := envExporterList("OTEL_METRICS_EXPORTER", "otlp"); got != nil || err != nil {
		t.Fatalf("expected nil when unset/empty, got %#v, %v", got, err)
	}
}

func TestResolveOTLPEndpoint(t *testing.T) {
	t.Setenv("OTEL_EXPORTER_OTLP_ENDPOINT", "")
	t.Setenv("OTEL_EXPORTER_OTLP_TRACES_ENDPOINT", "")
	dest, err := resolveOTLPEndpoint("traces", "/v1/traces")
	if err != nil {
		t.Fatal(err)
	}
	if dest != nil {
		t.Fatalf("expected nil dest, got %#v", dest)
	}

	t.Setenv("OTEL_EXPORTER_OTLP_ENDPOINT", "http://localhost:4318")
	dest, err = resolveOTLPEndpoint("traces", "/v1/traces")
	if err != nil {
		t.Fatal(err)
	}
	if dest == nil || dest.host != "localhost:4318" || dest.path != "/v1/traces" || !dest.insecure {
		t.Fatalf("base endpoint: %#v", dest)
	}

	t.Setenv("OTEL_EXPORTER_OTLP_TRACES_ENDPOINT", "https://collector.example/v1/traces")
	dest, err = resolveOTLPEndpoint("traces", "/v1/traces")
	if err != nil {
		t.Fatal(err)
	}
	if dest == nil || dest.host != "collector.example" || dest.path != "/v1/traces" || dest.insecure {
		t.Fatalf("per-signal override: %#v", dest)
	}
}

// Each set-but-invalid value in telemetry.md's "Invalid values" table stops Init
// (and so the service) with an error naming the variable.
func TestInitRefusesInvalidValues(t *testing.T) {
	tests := []struct{ key, value string }{
		{"OTEL_TRACES_EXPORTER", "jaeger"},
		{"OTEL_TRACES_EXPORTER", "otlp,prometheus"},
		{"OTEL_METRICS_EXPORTER", "otlp,statsd"},
		{"OTEL_EXPORTER_OTLP_PROTOCOL", "grpc"},
		{"OTEL_EXPORTER_OTLP_TRACES_PROTOCOL", "http/json"},
		{"OTEL_EXPORTER_OTLP_METRICS_PROTOCOL", "grpc"},
		{"OTEL_EXPORTER_OTLP_ENDPOINT", "localhost:4318"},
		{"OTEL_EXPORTER_OTLP_ENDPOINT", "grpc://collector:4317"},
		{"OTEL_EXPORTER_OTLP_TRACES_ENDPOINT", "ftp://collector/v1/traces"},
		{"OTEL_EXPORTER_OTLP_METRICS_ENDPOINT", "collector"},
		{"OTEL_TRACES_SAMPLER_RATIO", "1.5"},
		{"OTEL_TRACES_SAMPLER_RATIO", "-0.1"},
		{"OTEL_TRACES_SAMPLER_RATIO", "half"},
		{"OTEL_METRIC_EXPORT_INTERVAL", "0"},
		{"OTEL_METRIC_EXPORT_INTERVAL", "10s"},
		{"OTEL_SDK_DISABLED", "yes"},
		{"OTEL_RESOURCE_ATTRIBUTES", "service.namespace"},
		{"OTEL_RESOURCE_ATTRIBUTES", "a=b,=c"},
	}
	for _, tt := range tests {
		t.Run(tt.key+"="+tt.value, func(t *testing.T) {
			t.Setenv(tt.key, tt.value)
			_, err := Init(context.Background())
			if err == nil {
				t.Fatalf("Init accepted %s=%q", tt.key, tt.value)
			}
			if !strings.Contains(err.Error(), tt.key) {
				t.Fatalf("error %q does not name %s", err, tt.key)
			}
		})
	}
}

func TestInitAcceptsValidValues(t *testing.T) {
	for key, value := range map[string]string{
		"OTEL_TRACES_EXPORTER":               "otlp,none",
		"OTEL_METRICS_EXPORTER":              "prometheus,none",
		"OTEL_EXPORTER_OTLP_PROTOCOL":        "http/protobuf",
		"OTEL_EXPORTER_OTLP_ENDPOINT":        "https://collector.example",
		"OTEL_TRACES_SAMPLER_RATIO":          "0.25",
		"OTEL_METRIC_EXPORT_INTERVAL":        "5000",
		"OTEL_SDK_DISABLED":                  "true",
		"OTEL_RESOURCE_ATTRIBUTES":           "service.namespace=edge,deployment.environment=dev",
		"OTEL_EXPORTER_OTLP_TRACES_ENDPOINT": "",
	} {
		t.Setenv(key, value)
	}
	if _, err := Init(context.Background()); err != nil {
		t.Fatal(err)
	}
}

// A failed export reaches OpenTelemetry's error handler, which writes one JSON line.
func TestErrorHandlerLogsJSON(t *testing.T) {
	var buf bytes.Buffer
	prev := otel.GetErrorHandler()
	otel.SetErrorHandler(ErrorHandler(zerolog.New(&buf)))
	t.Cleanup(func() { otel.SetErrorHandler(prev) })

	otel.Handle(errors.New("traces export: connection refused"))

	var line map[string]any
	if err := json.Unmarshal(bytes.TrimSpace(buf.Bytes()), &line); err != nil {
		t.Fatalf("not one JSON line: %q", buf.String())
	}
	if line["level"] != "error" || line["message"] != "telemetry error" || line["error_message"] != "traces export: connection refused" {
		t.Fatalf("line = %v", line)
	}
}

func serviceVersion(t *testing.T) string {
	t.Helper()
	res, err := buildResource(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	v, _ := res.Set().Value(semconv.ServiceVersionKey)
	return v.AsString()
}

// service.version: OTEL_SERVICE_VERSION, then OTEL_RESOURCE_ATTRIBUTES, then the
// compiled Version, which is dev without a VERSION at build time.
func TestServiceVersion(t *testing.T) {
	t.Setenv("OTEL_SERVICE_VERSION", "")
	t.Setenv("OTEL_RESOURCE_ATTRIBUTES", "")
	t.Setenv("CI_COMMIT_TAG", "v9.9.9")

	if got := serviceVersion(t); got != "dev" {
		t.Fatalf("without VERSION: %q, want dev", got)
	}

	prev := Version
	Version = "v1.2.3"
	t.Cleanup(func() { Version = prev })
	if got := serviceVersion(t); got != "v1.2.3" {
		t.Fatalf("with VERSION: %q, want v1.2.3", got)
	}

	t.Setenv("OTEL_RESOURCE_ATTRIBUTES", "service.version=v2.0.0")
	if got := serviceVersion(t); got != "v2.0.0" {
		t.Fatalf("OTEL_RESOURCE_ATTRIBUTES: %q, want v2.0.0", got)
	}

	t.Setenv("OTEL_SERVICE_VERSION", "v3.0.0")
	if got := serviceVersion(t); got != "v3.0.0" {
		t.Fatalf("OTEL_SERVICE_VERSION: %q, want v3.0.0", got)
	}
}
