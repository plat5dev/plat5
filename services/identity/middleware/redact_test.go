package middleware

import (
	"context"
	"net/http/httptest"
	"testing"

	"github.com/gofiber/contrib/v3/otel"
	"github.com/gofiber/fiber/v3"
	"go.opentelemetry.io/otel/attribute"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"

	apierrors "github.com/plat5dev/plat5/identity/errors"
	"github.com/plat5dev/plat5/identity/telemetry"
)

func TestRedactQuery(t *testing.T) {
	t.Parallel()
	tests := map[string]string{
		"a=1&b=2":     "a=1&b=2",
		"sig=abc&a=1": "sig=REDACTED&a=1",
		"X-Amz-Signature=x&X-Amz-Credential=y&X-Amz-Security-Token=z": "X-Amz-Signature=REDACTED&X-Amz-Credential=REDACTED&X-Amz-Security-Token=REDACTED",
		"Signature=s&AWSAccessKeyId=k&X-Goog-Signature=g":             "Signature=REDACTED&AWSAccessKeyId=REDACTED&X-Goog-Signature=REDACTED",
		"SIG=abc&sig":    "SIG=abc&sig",
		"q=a%20b+c&sig=": "q=a%20b+c&sig=REDACTED",
	}
	for in, want := range tests {
		if got := RedactQuery(in); got != want {
			t.Errorf("RedactQuery(%q) = %q, want %q", in, got, want)
		}
	}
}

// serve runs one request through the services' span stack (the fiber otel
// middleware on HTTPServerTracerProvider, then RequestLogger) and returns its span.
func serve(t *testing.T, target string) sdktrace.ReadOnlySpan {
	t.Helper()
	telem, err := telemetry.Init(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	rec := tracetest.NewSpanRecorder()
	tp := sdktrace.NewTracerProvider(sdktrace.WithSpanProcessor(rec))

	app := fiber.New(fiber.Config{ErrorHandler: apierrors.FiberErrorHandler})
	app.Use(otel.Middleware(
		otel.WithTracerProvider(HTTPServerTracerProvider(tp)),
		otel.WithoutMetrics(true),
		otel.WithSpanNameFormatter(HTTPSpanName),
	))
	app.Use(RequestLogger(telem))
	app.Get("/ok", func(c fiber.Ctx) error { return c.SendStatus(fiber.StatusOK) })
	app.Get("/missing", func(c fiber.Ctx) error { return apierrors.NotFoundError("node", "n1") })
	app.Get("/broken", func(c fiber.Ctx) error { return apierrors.InternalError() })

	if _, err := app.Test(httptest.NewRequest("GET", target, nil)); err != nil {
		t.Fatal(err)
	}
	spans := rec.Ended()
	if len(spans) != 1 {
		t.Fatalf("got %d spans, want 1", len(spans))
	}
	return spans[0]
}

func attrs(span sdktrace.ReadOnlySpan) map[attribute.Key]string {
	out := map[attribute.Key]string{}
	for _, kv := range span.Attributes() {
		out[kv.Key] = kv.Value.Emit()
	}
	return out
}

func TestHTTPServerSpanRedactsQuery(t *testing.T) {
	a := attrs(serve(t, "/ok?sig=secret&page=2&X-Amz-Signature=s3"))
	if got := a["url.query"]; got != "sig=REDACTED&page=2&X-Amz-Signature=REDACTED" {
		t.Fatalf("url.query = %q", got)
	}
	if got := a["url.full"]; got != "/ok?sig=REDACTED&page=2&X-Amz-Signature=REDACTED" {
		t.Fatalf("url.full = %q", got)
	}
	if a["url.scheme"] != "http" || a["url.path"] != "/ok" {
		t.Fatalf("url.scheme = %q, url.path = %q", a["url.scheme"], a["url.path"])
	}
}

func TestHTTPServerSpanWithoutQueryHasNoURLQuery(t *testing.T) {
	if q, ok := attrs(serve(t, "/ok"))["url.query"]; ok {
		t.Fatalf("url.query = %q on a request without a query", q)
	}
}

func TestErrorTypeOnlyOn5xx(t *testing.T) {
	broken := serve(t, "/broken")
	if got := attrs(broken)["error.type"]; got != "internal" {
		t.Fatalf("5xx error.type = %q, want internal", got)
	}
	if broken.Status().Code.String() != "Error" || broken.Status().Description != "" {
		t.Fatalf("5xx status = %v", broken.Status())
	}

	missing := serve(t, "/missing")
	if got, ok := attrs(missing)["error.type"]; ok {
		t.Fatalf("4xx has error.type %q", got)
	}
	if missing.Status().Code.String() != "Unset" {
		t.Fatalf("4xx status = %v", missing.Status())
	}
}
