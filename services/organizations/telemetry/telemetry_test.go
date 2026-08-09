package telemetry

import (
	"testing"
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
	got := envExporterList("OTEL_METRICS_EXPORTER")
	if len(got) != 2 || got[0] != "otlp" || got[1] != "prometheus" {
		t.Fatalf("envExporterList = %#v", got)
	}

	t.Setenv("OTEL_METRICS_EXPORTER", "")
	if envExporterList("OTEL_METRICS_EXPORTER") != nil {
		t.Fatal("expected nil when unset/empty")
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
