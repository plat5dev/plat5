// Command audit stores Plat5's customer and user audit logs and serves them back.
// Contract: docs/concepts/audit.md, docs/internals/audit-storage.md.
//
//	audit migrate   as the schema owner: migrations, then the writer's grants
//	audit serve     as the writer: the internal write/export API and read routes
//	audit prune     as the schema owner: drop monthly partitions older than cutoff
package main

import (
	"context"
	"flag"
	"fmt"
	"io"
	"log"
	"os"
	"os/signal"
	"sync"
	"syscall"
	"time"

	"github.com/gofiber/contrib/v3/otel"
	"github.com/gofiber/fiber/v3"
	"github.com/gofiber/fiber/v3/middleware/adaptor"
	"github.com/gofiber/fiber/v3/middleware/recover"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/plat5dev/plat5/audit/config"
	"github.com/plat5dev/plat5/audit/db"
	apierrors "github.com/plat5dev/plat5/audit/errors"
	"github.com/plat5dev/plat5/audit/events"
	"github.com/plat5dev/plat5/audit/metrics"
	"github.com/plat5dev/plat5/audit/middleware"
	"github.com/plat5dev/plat5/audit/telemetry"
)

// How often partitions are checked. They run months ahead, so this only has
// to happen well inside a month.
const partitionInterval = time.Hour

func main() {
	cmd := ""
	var args []string
	if len(os.Args) > 1 {
		cmd = os.Args[1]
		args = os.Args[2:]
	}
	if err := run(cmd, args); err != nil {
		log.Fatalf("audit %s: %v", cmd, err)
	}
}

func run(cmd string, args []string) error {
	cfg, err := config.Load(cmd)
	if err != nil {
		return err
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	switch cmd {
	case "migrate":
		if len(args) > 0 {
			return fmt.Errorf("unexpected arguments: %v", args)
		}
		pool, err := db.Connect(ctx, cfg.DatabaseURL)
		if err != nil {
			return fmt.Errorf("failed to connect to postgres: %w", err)
		}
		defer pool.Close()
		if err := db.Migrate(ctx, pool, cfg.WriterRole); err != nil {
			return fmt.Errorf("failed to migrate database: %w", err)
		}
		log.Printf("migrated audit schema version=%s writer_role=%s", db.LatestVersion(), cfg.WriterRole)
		return nil

	case "prune":
		fs := flag.NewFlagSet("prune", flag.ContinueOnError)
		fs.SetOutput(io.Discard)
		before := fs.String("before", "", "drop monthly partitions strictly older than YYYY-MM (UTC)")
		olderThan := fs.String("older-than", "", "drop monthly partitions strictly older than <N>m (e.g. 12m)")
		dryRun := fs.Bool("dry-run", false, "list partitions that would be dropped without dropping them")
		if err := fs.Parse(args); err != nil {
			return err
		}
		if fs.NArg() > 0 {
			return fmt.Errorf("unexpected arguments: %v", fs.Args())
		}
		cutoff, err := db.ParsePruneCutoff(*before, *olderThan, time.Now())
		if err != nil {
			return err
		}
		pool, err := db.Connect(ctx, cfg.DatabaseURL)
		if err != nil {
			return fmt.Errorf("failed to connect to postgres: %w", err)
		}
		defer pool.Close()
		dropped, err := db.Prune(ctx, pool, cutoff, *dryRun)
		if err != nil {
			return err
		}
		log.Printf("pruned audit partitions cutoff=%s dry_run=%t count=%d partitions=%v",
			cutoff.Format("2006-01"), *dryRun, len(dropped), dropped)
		return nil

	case "serve":
		if len(args) > 0 {
			return fmt.Errorf("unexpected arguments: %v", args)
		}
		return serve(ctx, stop, cfg)

	default:
		return fmt.Errorf("usage: audit migrate|serve|prune")
	}
}

func serve(runCtx context.Context, stop context.CancelFunc, cfg config.Config) error {
	// Register prometheus metrics before OTLP bridge so the first export sees them.
	metrics.Init()

	telem, err := telemetry.Init(runCtx)
	if err != nil {
		return fmt.Errorf("failed to initialize telemetry: %w", err)
	}

	pool, err := db.Connect(runCtx, cfg.DatabaseURL)
	if err != nil {
		return fmt.Errorf("failed to connect to postgres: %w", err)
	}
	defer pool.Close()

	// The role that serves can change or delete no history (docs/internals/audit-storage.md#roles).
	if err := db.CheckWriter(runCtx, pool); err != nil {
		return err
	}

	store := events.NewStore(pool)
	if err := store.EnsureAround(runCtx, time.Now()); err != nil {
		return fmt.Errorf("failed to create audit partitions: %w", err)
	}
	handler := events.NewHandler(store)

	app := newPublicApp(telem, handler)
	internalApp := newInternalApp(telem, pool, handler)

	baseLogger := telem.Logger()
	baseLogger.Info().
		Str("port", cfg.Port).
		Str("internal_port", cfg.InternalPort).
		Str("version", db.LatestVersion()).
		Msg("starting audit server")

	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		store.MaintainPartitions(runCtx, partitionInterval, func(err error) {
			baseLogger.Error().Str("error_type", apierrors.KindDB.String()).Err(err).Msg("audit partition maintenance failed")
		})
	}()

	errCh := make(chan error, 2)
	go func() {
		errCh <- internalApp.Listen(":" + cfg.InternalPort)
	}()
	go func() {
		errCh <- app.Listen(":" + cfg.Port)
	}()

	var listenErr error
	select {
	case <-runCtx.Done():
		baseLogger.Info().Msg("shutdown signal received")
	case err := <-errCh:
		stop()
		if err != nil {
			listenErr = err
			baseLogger.Error().Err(err).Msg("server exited unexpectedly")
		}
	}

	shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	if err := app.ShutdownWithContext(shutdownCtx); err != nil {
		baseLogger.Error().Err(err).Msg("public server shutdown")
	}
	if err := internalApp.ShutdownWithContext(shutdownCtx); err != nil {
		baseLogger.Error().Err(err).Msg("internal server shutdown")
	}
	wg.Wait()
	if err := telem.Shutdown(shutdownCtx); err != nil {
		baseLogger.Error().Err(err).Msg("telemetry shutdown")
	}
	baseLogger.Info().Msg("shutdown complete")
	return listenErr
}

func newPublicApp(telem *telemetry.Telemetry, handler *events.Handler) *fiber.App {
	app := fiber.New(fiber.Config{
		AppName:      "audit",
		ErrorHandler: apierrors.FiberErrorHandler,
	})
	app.Use(recover.New())
	app.Use(otel.Middleware(
		otel.WithTracerProvider(middleware.HTTPServerTracerProvider(telem.TracerProvider())),
		otel.WithPropagators(telem.Propagator()),
		otel.WithoutMetrics(true),
		otel.WithSpanNameFormatter(middleware.HTTPSpanName),
	))
	app.Use(middleware.RequestLogger(telem))

	handler.MountPublic(app)
	return app
}

func newInternalApp(
	telem *telemetry.Telemetry,
	pool *pgxpool.Pool,
	handler *events.Handler,
) *fiber.App {
	app := fiber.New(fiber.Config{
		AppName:      "audit-internal",
		ErrorHandler: apierrors.FiberErrorHandler,
	})
	app.Use(recover.New())

	app.Get("/health/live", func(c fiber.Ctx) error {
		return c.JSON(fiber.Map{"status": "healthy"})
	})
	app.Get("/health/ready", func(c fiber.Ctx) error {
		pingCtx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		if err := pool.Ping(pingCtx); err != nil {
			return c.Status(fiber.StatusServiceUnavailable).JSON(fiber.Map{"status": "unhealthy"})
		}
		return c.JSON(fiber.Map{"status": "healthy"})
	})
	app.Get("/metrics", adaptor.HTTPHandler(metrics.Handler()))

	// Health and /metrics stay off traces and HTTP RED (docs/reference/health.md).
	// The internal port takes no token: it's reachable only inside the
	// deployment's network (docs/reference/api/internal.md).
	internalAPI := app.Group("/internal",
		otel.Middleware(
			otel.WithTracerProvider(middleware.HTTPServerTracerProvider(telem.TracerProvider())),
			otel.WithPropagators(telem.Propagator()),
			otel.WithoutMetrics(true),
			otel.WithSpanNameFormatter(middleware.HTTPSpanName),
		),
		middleware.RequestLogger(telem),
	)
	handler.MountInternal(internalAPI)
	return app
}
