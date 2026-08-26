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
	dbNamespace  = "identity"

	// KeyScope labels for api_keys_* metrics (separate products, shared counters).
	KeyScopeUser   = "user"
	KeyScopeMember = "member"
)

var (
	initOnce        sync.Once
	requestDuration *prometheus.HistogramVec
	requestsTotal   *prometheus.CounterVec
	orgsCreated     prometheus.Counter
	memberOps       *prometheus.CounterVec
	inviteOps       *prometheus.CounterVec
	resolveTotal    *prometheus.CounterVec
	keysCreated     *prometheus.CounterVec
	keysRevoked     *prometheus.CounterVec
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
			Help:    "HTTP request latency observed by the identity service",
			Buckets: []float64{0.005, 0.01, 0.025, 0.05, 0.1, 0.25, 0.5, 1, 2.5, 5},
		}, []string{"route", "method"})

		requestsTotal = prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "http_requests_total",
			Help: "Total HTTP requests handled by the identity service",
		}, []string{"route", "method", "status"})

		orgsCreated = prometheus.NewCounter(prometheus.CounterOpts{
			Name: "organizations_created_total",
			Help: "Total organizations created",
		})

		memberOps = prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "member_operations_total",
			Help: "Member mutations by operation",
		}, []string{"operation"})

		inviteOps = prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "invite_operations_total",
			Help: "Organization invite mutations by operation",
		}, []string{"operation"})

		resolveTotal = prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "member_resolve_total",
			Help: "Internal member resolve outcomes",
		}, []string{"result"})

		keysCreated = prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "api_keys_created_total",
			Help: "Total API keys created",
		}, []string{"key_scope"})

		keysRevoked = prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "api_keys_revoked_total",
			Help: "Total API keys revoked",
		}, []string{"key_scope"})

		keysValidated = prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "api_keys_validated_total",
			Help: "API key validate outcomes",
		}, []string{"key_scope", "valid"})

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
			orgsCreated,
			memberOps,
			inviteOps,
			resolveTotal,
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

func RecordOrgCreated() {
	Init()
	orgsCreated.Inc()
}

func RecordMemberOp(operation string) {
	Init()
	memberOps.WithLabelValues(operation).Inc()
}

func RecordInviteOp(operation string) {
	Init()
	inviteOps.WithLabelValues(operation).Inc()
}

func RecordResolve(result string) {
	Init()
	resolveTotal.WithLabelValues(result).Inc()
}

func RecordKeyCreated(keyScope string) {
	Init()
	keysCreated.WithLabelValues(keyScope).Inc()
}

func RecordKeyRevoked(keyScope string) {
	Init()
	keysRevoked.WithLabelValues(keyScope).Inc()
}

func RecordKeyValidation(keyScope string, valid bool) {
	Init()
	keysValidated.WithLabelValues(keyScope, fmt.Sprintf("%t", valid)).Inc()
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
