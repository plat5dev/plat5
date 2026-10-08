package metrics

import (
	"fmt"
	"net/http"
	"sync"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promhttp"
)

const (
	dbSystemName = "postgresql"
	dbNamespace  = "audit"

	// Intent results (audit_intents_total).
	IntentCreated  = "created"
	IntentExisting = "existing"

	// Outcome results (audit_outcomes_total).
	OutcomeApplied  = "applied"
	OutcomeFinal    = "already_final"
	OutcomeNotFound = "not_found"
)

var (
	initOnce        sync.Once
	requestDuration *prometheus.HistogramVec
	requestsTotal   *prometheus.CounterVec
	intentsTotal    *prometheus.CounterVec
	outcomesTotal   *prometheus.CounterVec
	dbOpsTotal      *prometheus.CounterVec
	dbOpsErrors     *prometheus.CounterVec
	dbOpsDuration   *prometheus.HistogramVec
)

// Init registers service metrics on the default Prometheus registry.
// Process metrics (RSS, CPU, start time) come free from client_golang's default registry.
func Init() {
	initOnce.Do(func() {
		requestDuration = prometheus.NewHistogramVec(prometheus.HistogramOpts{
			Name:    "http_request_duration_seconds",
			Help:    "HTTP request latency observed by the audit service",
			Buckets: []float64{0.005, 0.01, 0.025, 0.05, 0.1, 0.25, 0.5, 1, 2.5, 5},
		}, []string{"route", "method"})

		requestsTotal = prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "http_requests_total",
			Help: "Total HTTP requests handled by the audit service",
		}, []string{"route", "method", "status"})

		intentsTotal = prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "audit_intents_total",
			Help: "Audit intents by result (created, or existing for a retry)",
		}, []string{"result"})

		outcomesTotal = prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "audit_outcomes_total",
			Help: "Audit outcomes by result (applied, already_final, not_found)",
		}, []string{"result"})

		dbLabels := []string{"db_system_name", "db_operation_name", "db_namespace"}

		dbOpsTotal = prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "db_operations_total",
			Help: "Total database operations",
		}, dbLabels)

		dbOpsErrors = prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "db_operation_errors_total",
			Help: "Total failed database operations",
		}, dbLabels)

		dbOpsDuration = prometheus.NewHistogramVec(prometheus.HistogramOpts{
			Name:    "db_operation_duration_seconds",
			Help:    "Database operation duration in seconds",
			Buckets: []float64{0.001, 0.005, 0.01, 0.025, 0.05, 0.1, 0.25, 0.5, 1},
		}, dbLabels)

		prometheus.MustRegister(
			requestDuration,
			requestsTotal,
			intentsTotal,
			outcomesTotal,
			dbOpsTotal,
			dbOpsErrors,
			dbOpsDuration,
		)
	})
}

func Handler() http.Handler {
	Init()
	return promhttp.Handler()
}

func ObserveRequest(route, method string, status int, duration time.Duration) {
	Init()
	if route == "" {
		route = "unknown"
	}
	if method == "" {
		method = "UNKNOWN"
	}
	requestsTotal.WithLabelValues(route, method, fmt.Sprintf("%d", status)).Inc()
	requestDuration.WithLabelValues(route, method).Observe(duration.Seconds())
}

func RecordIntent(result string) {
	Init()
	intentsTotal.WithLabelValues(result).Inc()
}

func RecordOutcome(result string) {
	Init()
	outcomesTotal.WithLabelValues(result).Inc()
}

func RecordDBOperation(operation string, duration time.Duration, err error) {
	Init()
	if operation == "" {
		operation = "unknown"
	}
	labels := []string{dbSystemName, operation, dbNamespace}
	dbOpsTotal.WithLabelValues(labels...).Inc()
	dbOpsDuration.WithLabelValues(labels...).Observe(duration.Seconds())
	if err != nil {
		dbOpsErrors.WithLabelValues(labels...).Inc()
	}
}
