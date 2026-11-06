package middleware

import (
	"context"
	"strings"

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/trace"
	"go.opentelemetry.io/otel/trace/embedded"
)

// redactedQueryKeys are the query keys whose values url.query never carries
// (docs/reference/telemetry.md, HTTP server spans). Keys match exactly.
var redactedQueryKeys = map[string]bool{
	"sig":                  true,
	"Signature":            true,
	"AWSAccessKeyId":       true,
	"X-Amz-Signature":      true,
	"X-Amz-Credential":     true,
	"X-Amz-Security-Token": true,
	"X-Goog-Signature":     true,
}

// RedactQuery returns the raw query with the value of each redacted key
// replaced by REDACTED. Every other key and value is kept as sent.
func RedactQuery(query string) string {
	parts := strings.Split(query, "&")
	for i, part := range parts {
		if key, _, ok := strings.Cut(part, "="); ok && redactedQueryKeys[key] {
			parts[i] = key + "=REDACTED"
		}
	}
	return strings.Join(parts, "&")
}

// HTTPServerTracerProvider wraps the provider the fiber otel middleware starts
// HTTP server spans with. The middleware always sets url.query (empty without a
// query) and url.full, both unredacted; this rewrites both from url.full, the
// request URI as sent: url.query is the redacted query, or absent without one.
func HTTPServerTracerProvider(tp trace.TracerProvider) trace.TracerProvider {
	return &redactingProvider{inner: tp}
}

type redactingProvider struct {
	embedded.TracerProvider
	inner trace.TracerProvider
}

func (p *redactingProvider) Tracer(name string, opts ...trace.TracerOption) trace.Tracer {
	return &redactingTracer{inner: p.inner.Tracer(name, opts...)}
}

type redactingTracer struct {
	embedded.Tracer
	inner trace.Tracer
}

func (t *redactingTracer) Start(ctx context.Context, name string, opts ...trace.SpanStartOption) (context.Context, trace.Span) {
	cfg := trace.NewSpanStartConfig(opts...)
	rebuilt := []trace.SpanStartOption{
		trace.WithAttributes(redactURLAttributes(cfg.Attributes())...),
		trace.WithSpanKind(cfg.SpanKind()),
		trace.WithLinks(cfg.Links()...),
	}
	if cfg.NewRoot() {
		rebuilt = append(rebuilt, trace.WithNewRoot())
	}
	if ts := cfg.Timestamp(); !ts.IsZero() {
		rebuilt = append(rebuilt, trace.WithTimestamp(ts))
	}
	return t.inner.Start(ctx, name, rebuilt...)
}

func redactURLAttributes(attrs []attribute.KeyValue) []attribute.KeyValue {
	full := ""
	for _, kv := range attrs {
		if kv.Key == "url.full" {
			full = kv.Value.AsString()
		}
	}
	base, query, hasQuery := strings.Cut(full, "?")
	out := make([]attribute.KeyValue, 0, len(attrs))
	for _, kv := range attrs {
		switch kv.Key {
		case "url.query":
			continue
		case "url.full":
			if hasQuery {
				kv = attribute.String("url.full", base+"?"+RedactQuery(query))
			}
		}
		out = append(out, kv)
	}
	if hasQuery && query != "" {
		out = append(out, attribute.String("url.query", RedactQuery(query)))
	}
	return out
}
