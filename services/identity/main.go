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
	"github.com/plat5dev/plat5/identity/memberkeys"
	"github.com/plat5dev/plat5/identity/metrics"
	"github.com/plat5dev/plat5/identity/middleware"
	"github.com/plat5dev/plat5/identity/orgs"
	"github.com/plat5dev/plat5/identity/telemetry"
	"github.com/plat5dev/plat5/identity/userkeys"
)

func main() {
	cfg := config.Load()
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

	orgStore := orgs.NewStore(pool)
	orgHandler := orgs.NewHandler(orgStore, telem)
	userKeyHandler := userkeys.NewHandler(userkeys.NewStore(pool), telem)
	memberKeyHandler := memberkeys.NewHandler(memberkeys.NewStore(pool), orgStore, telem)

	app := newPublicApp(telem, orgHandler, userKeyHandler, memberKeyHandler)
	internalApp := newInternalApp(telem, pool, cfg.InternalAuthToken, orgHandler, userKeyHandler, memberKeyHandler)

	baseLogger := telem.Logger()
	baseLogger.Info().
		Str("port", cfg.Port).
		Str("internal_port", cfg.InternalPort).
		Msg("starting identity server")

	runCtx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

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

func newPublicApp(
	telem *telemetry.Telemetry,
	orgHandler *orgs.Handler,
	userKeyHandler *userkeys.Handler,
	memberKeyHandler *memberkeys.Handler,
) *fiber.App {
	app := fiber.New(fiber.Config{
		AppName:      "identity",
		ErrorHandler: apierrors.FiberErrorHandler,
	})
	app.Use(recover.New())
	app.Use(otel.Middleware(
		otel.WithTracerProvider(telem.TracerProvider()),
		otel.WithPropagators(telem.Propagator()),
		otel.WithoutMetrics(true),
	))
	app.Use(middleware.RequestLogger(telem))

	userKeyHandler.MountPublic(app.Group("/api/users", middleware.RequireUserID()))
	orgsGroup := app.Group("/api/organizations", middleware.RequireUserID())
	orgHandler.MountPublic(orgsGroup)
	memberKeyHandler.MountPublic(orgsGroup)
	return app
}

func newInternalApp(
	telem *telemetry.Telemetry,
	pool *pgxpool.Pool,
	internalToken string,
	orgHandler *orgs.Handler,
	userKeyHandler *userkeys.Handler,
	memberKeyHandler *memberkeys.Handler,
) *fiber.App {
	app := fiber.New(fiber.Config{
		AppName:      "identity-internal",
		ErrorHandler: apierrors.FiberErrorHandler,
	})
	app.Use(recover.New())
	app.Use(otel.Middleware(
		otel.WithTracerProvider(telem.TracerProvider()),
		otel.WithPropagators(telem.Propagator()),
		otel.WithoutMetrics(true),
	))
	app.Use(middleware.RequestLogger(telem))

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

	internalAPI := app.Group("/internal", middleware.RequireInternalToken(internalToken))
	orgHandler.MountInternal(internalAPI)
	userKeyHandler.MountInternal(internalAPI)
	memberKeyHandler.MountInternal(internalAPI)
	return app
}
