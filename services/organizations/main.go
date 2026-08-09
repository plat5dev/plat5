package main

import (
	"context"
	"log"
	"os"
	"time"

	"github.com/gofiber/contrib/v3/otel"
	"github.com/gofiber/fiber/v3"
	"github.com/gofiber/fiber/v3/middleware/adaptor"
	"github.com/gofiber/fiber/v3/middleware/recover"

	"github.com/plat5dev/plat5/organizations/db"
	"github.com/plat5dev/plat5/organizations/errors"
	"github.com/plat5dev/plat5/organizations/metrics"
	"github.com/plat5dev/plat5/organizations/middleware"
	"github.com/plat5dev/plat5/organizations/orgs"
	"github.com/plat5dev/plat5/organizations/telemetry"
)

func main() {
	ctx := context.Background()
	// Register prometheus metrics before OTLP bridge so the first export sees them.
	metrics.Init()

	telem, err := telemetry.Init(ctx)
	if err != nil {
		log.Fatalf("failed to initialize telemetry: %v", err)
	}
	defer func() {
		if err := telem.Shutdown(context.Background()); err != nil {
			log.Printf("error shutting down telemetry: %v", err)
		}
	}()

	pool, err := db.Connect(ctx)
	if err != nil {
		log.Fatalf("failed to connect to postgres: %v", err)
	}
	defer pool.Close()

	if err := db.Migrate(ctx, pool); err != nil {
		log.Fatalf("failed to migrate database: %v", err)
	}

	store := orgs.NewStore(pool)
	handler := orgs.NewHandler(store, telem)

	app := fiber.New(fiber.Config{
		AppName:      "organizations",
		ErrorHandler: errors.FiberErrorHandler,
	})

	app.Use(recover.New())
	app.Use(otel.Middleware(
		otel.WithTracerProvider(telem.TracerProvider()),
		otel.WithPropagators(telem.Propagator()),
		otel.WithoutMetrics(true),
	))
	app.Use(middleware.RequestLogger(telem))

	api := app.Group("/api/organizations")
	api.Post("/", middleware.RequireUserID(), handler.CreateOrganization)
	api.Get("/", middleware.RequireUserID(), handler.ListOrganizations)
	api.Get("/:organization_id", middleware.RequireUserID(), handler.GetOrganization)
	api.Patch("/:organization_id", middleware.RequireUserID(), handler.UpdateOrganization)
	api.Delete("/:organization_id", middleware.RequireUserID(), handler.DeleteOrganization)

	api.Get("/:organization_id/memberships", middleware.RequireUserID(), handler.ListMemberships)
	api.Post("/:organization_id/memberships", middleware.RequireUserID(), handler.CreateMembership)
	api.Get("/:organization_id/memberships/:membership_id", middleware.RequireUserID(), handler.GetMembership)
	api.Patch("/:organization_id/memberships/:membership_id", middleware.RequireUserID(), handler.UpdateMembership)
	api.Delete("/:organization_id/memberships/:membership_id", middleware.RequireUserID(), handler.DeleteMembership)

	port := os.Getenv("PORT")
	if port == "" {
		port = "3000"
	}

	internalApp := fiber.New(fiber.Config{
		AppName:      "organizations-internal",
		ErrorHandler: errors.FiberErrorHandler,
	})

	internalApp.Get("/health/live", func(c fiber.Ctx) error {
		return c.JSON(fiber.Map{"status": "healthy"})
	})

	internalApp.Get("/health/ready", func(c fiber.Ctx) error {
		pingCtx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		if err := pool.Ping(pingCtx); err != nil {
			return c.Status(fiber.StatusServiceUnavailable).JSON(fiber.Map{"status": "unhealthy"})
		}
		return c.JSON(fiber.Map{"status": "healthy"})
	})

	metricsHandler := adaptor.HTTPHandler(metrics.Handler())
	internalApp.Get("/metrics", metricsHandler)

	internalApp.Post("/internal/memberships/resolve", middleware.RequireInternalToken(), handler.Resolve)

	internalPort := os.Getenv("INTERNAL_PORT")
	if internalPort == "" {
		internalPort = "3001"
	}

	baseLogger := telem.Logger()
	baseLogger.Info().
		Str("port", port).
		Str("internal_port", internalPort).
		Msg("starting organizations server")

	go func() {
		if err := internalApp.Listen(":" + internalPort); err != nil {
			baseLogger.Fatal().Err(err).Msg("internal server exited")
		}
	}()

	if err := app.Listen(":" + port); err != nil {
		baseLogger.Fatal().Err(err).Msg("fiber server exited")
	}
}
