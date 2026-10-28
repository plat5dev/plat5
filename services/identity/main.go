package main

import (
	"context"
	"log"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/gofiber/contrib/v3/otel"
	"github.com/gofiber/fiber/v3"
	"github.com/gofiber/fiber/v3/middleware/adaptor"
	"github.com/gofiber/fiber/v3/middleware/recover"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/plat5dev/plat5/identity/config"
	"github.com/plat5dev/plat5/identity/db"
	apierrors "github.com/plat5dev/plat5/identity/errors"
	"github.com/plat5dev/plat5/identity/feed"
	"github.com/plat5dev/plat5/identity/lifecycle"
	"github.com/plat5dev/plat5/identity/members"
	"github.com/plat5dev/plat5/identity/metrics"
	"github.com/plat5dev/plat5/identity/middleware"
	"github.com/plat5dev/plat5/identity/nodes"
	"github.com/plat5dev/plat5/identity/telemetry"
)

func main() {
	cfg, err := config.Load()
	if err != nil {
		log.Fatalf("invalid identity configuration: %v", err)
	}
	ctx := context.Background()

	// Register prometheus metrics before OTLP bridge so the first export sees them.
	metrics.Init()

	telem, err := telemetry.Init(ctx)
	if err != nil {
		log.Fatalf("failed to initialize telemetry: %v", err)
	}

	pool, err := db.Connect(ctx, cfg.DatabaseURL)
	if err != nil {
		log.Fatalf("failed to connect to postgres: %v", err)
	}

	if err := db.Migrate(ctx, pool); err != nil {
		pool.Close()
		log.Fatalf("failed to migrate database: %v", err)
	}

	baseLogger := telem.Logger()
	runCtx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	// Identity serves nothing until it has the control plane's config
	// (docs/concepts/config.md#how-a-change-reaches-readers).
	src := &feed.Source{}
	follower := feed.NewFollower(cfg.ControlPlaneURL, src, baseLogger)
	if err := follower.Load(runCtx); err != nil {
		pool.Close()
		baseLogger.Info().Err(err).Msg("stopped before config loaded")
		return
	}
	nodeStore := nodes.NewStore(pool)
	memberStore := members.NewStore(pool, nodeStore, cfg.APIKeyBrand)
	sweeper := nodes.NewSweeper(nodeStore, baseLogger)
	sweeper.Also("expire invites", memberStore.ExpireInvites)
	go follower.Follow(runCtx)
	go sweeper.Run(runCtx)

	nodeHandler := nodes.NewHandler(nodeStore, src)
	memberHandler := members.NewHandler(memberStore, src)
	lifecycleHandler := lifecycle.NewHandler(lifecycle.NewStore(pool))

	app := newPublicApp(telem, nodeHandler, memberHandler)
	internalApp := newInternalApp(telem, pool, src, lifecycleHandler, nodeHandler, memberHandler)

	baseLogger.Info().
		Str("port", cfg.Port).
		Str("internal_port", cfg.InternalPort).
		Str("apikey_brand", cfg.APIKeyBrand).
		Int64("revision", src.Current().Revision).
		Msg("starting identity server")

	errCh := make(chan error, 2)
	go func() {
		errCh <- internalApp.Listen(":" + cfg.InternalPort)
	}()
	go func() {
		errCh <- app.Listen(":" + cfg.Port)
	}()

	select {
	case <-runCtx.Done():
		baseLogger.Info().Msg("shutdown signal received")
	case err := <-errCh:
		if err != nil {
			baseLogger.Error().Err(err).Msg("server exited unexpectedly")
			stop()
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
	pool.Close()
	if err := telem.Shutdown(shutdownCtx); err != nil {
		baseLogger.Error().Err(err).Msg("telemetry shutdown")
	}
	baseLogger.Info().Msg("shutdown complete")
}

func newPublicApp(telem *telemetry.Telemetry, nodeHandler *nodes.Handler, memberHandler *members.Handler) *fiber.App {
	app := fiber.New(fiber.Config{
		AppName:      "identity",
		ErrorHandler: apierrors.FiberErrorHandler,
	})
	app.Use(recover.New())
	app.Use(otel.Middleware(
		otel.WithTracerProvider(telem.TracerProvider()),
		otel.WithPropagators(telem.Propagator()),
		otel.WithoutMetrics(true),
		otel.WithSpanNameFormatter(middleware.HTTPSpanName),
	))
	app.Use(middleware.RequestLogger(telem))

	nodeHandler.Mount(app)
	memberHandler.Mount(app)
	return app
}

func newInternalApp(
	telem *telemetry.Telemetry,
	pool *pgxpool.Pool,
	src *feed.Source,
	lifecycleHandler *lifecycle.Handler,
	nodeHandler *nodes.Handler,
	memberHandler *members.Handler,
) *fiber.App {
	app := fiber.New(fiber.Config{
		AppName:      "identity-internal",
		ErrorHandler: apierrors.FiberErrorHandler,
	})
	app.Use(recover.New())

	app.Get("/health/live", func(c fiber.Ctx) error {
		return c.JSON(fiber.Map{"status": "healthy"})
	})
	app.Get("/health/ready", func(c fiber.Ctx) error {
		pingCtx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		if err := pool.Ping(pingCtx); err != nil || !src.Loaded() {
			return c.Status(fiber.StatusServiceUnavailable).JSON(fiber.Map{"status": "unhealthy"})
		}
		return c.JSON(fiber.Map{"status": "healthy"})
	})
	app.Get("/metrics", adaptor.HTTPHandler(metrics.Handler()))

	// Health and /metrics stay off traces and HTTP RED (docs/reference/health.md).
	// No token: internal ports are reachable only inside the deployment's
	// network (docs/reference/api/internal.md).
	traced := func(prefix string) fiber.Router {
		return app.Group(prefix,
			otel.Middleware(
				otel.WithTracerProvider(telem.TracerProvider()),
				otel.WithPropagators(telem.Propagator()),
				otel.WithoutMetrics(true),
				otel.WithSpanNameFormatter(middleware.HTTPSpanName),
			),
			middleware.RequestLogger(telem),
		)
	}
	internalAPI := traced("/internal")
	lifecycleHandler.MountInternal(internalAPI)
	memberHandler.MountInternal(internalAPI)
	nodeHandler.MountInternal(internalAPI)
	// AuthZEN's access evaluation API, for services, at AuthZEN's paths
	// (docs/reference/api/internal.md#access-evaluation).
	memberHandler.MountAccess(traced("/access/v1"))
	return app
}
