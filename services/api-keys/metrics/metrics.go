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
	dbNamespace  = "api_keys"
)

var (
	initOnce        sync.Once
	requestDuration *prometheus.HistogramVec
	requestsTotal   *prometheus.CounterVec
	keysCreated     prometheus.Counter
	keysRevoked     prometheus.Counter
	keysValidated   *prometheus.CounterVec
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
			Help:    "HTTP request latency observed by the api-keys service",
			Buckets: []float64{0.005, 0.01, 0.025, 0.05, 0.1, 0.25, 0.5, 1, 2.5, 5},
		}, []string{"route", "method"})

		requestsTotal = prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "http_requests_total",
			Help: "Total HTTP requests handled by the api-keys service",
		}, []string{"route", "method", "status"})

		keysCreated = prometheus.NewCounter(prometheus.CounterOpts{
			Name: "api_keys_created_total",
			Help: "Total API keys created",
		})

		keysRevoked = prometheus.NewCounter(prometheus.CounterOpts{
			Name: "api_keys_revoked_total",
			Help: "Total API keys revoked",
		})

		keysValidated = prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "api_keys_validated_total",
			Help: "Total API key validation requests",
		}, []string{"valid"})

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
			keysCreated,
			keysRevoked,
			keysValidated,
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

func RecordKeyCreated() {
	Init()
	keysCreated.Inc()
}

func RecordKeyRevoked() {
	Init()
	keysRevoked.Inc()
}

func RecordKeyValidation(valid bool) {
	Init()
	keysValidated.WithLabelValues(fmt.Sprintf("%t", valid)).Inc()
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
