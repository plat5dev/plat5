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

	"github.com/plat5dev/plat5/identity/db"
	"github.com/plat5dev/plat5/identity/errors"
	"github.com/plat5dev/plat5/identity/memberkeys"
	"github.com/plat5dev/plat5/identity/metrics"
	"github.com/plat5dev/plat5/identity/middleware"
	"github.com/plat5dev/plat5/identity/orgs"
	"github.com/plat5dev/plat5/identity/telemetry"
	"github.com/plat5dev/plat5/identity/userkeys"
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

	orgStore := orgs.NewStore(pool)
	orgHandler := orgs.NewHandler(orgStore, telem)
	userKeyStore := userkeys.NewStore(pool)
	userKeyHandler := userkeys.NewHandler(userKeyStore, telem)
	memberKeyStore := memberkeys.NewStore(pool)
	memberKeyHandler := memberkeys.NewHandler(memberKeyStore, orgStore, telem)

	app := fiber.New(fiber.Config{
		AppName:      "identity",
		ErrorHandler: errors.FiberErrorHandler,
	})

	app.Use(recover.New())
	app.Use(otel.Middleware(
		otel.WithTracerProvider(telem.TracerProvider()),
		otel.WithPropagators(telem.Propagator()),
		otel.WithoutMetrics(true),
	))
	app.Use(middleware.RequestLogger(telem))

	users := app.Group("/api/users")
	users.Post("/:user_id/api-keys", middleware.RequireUserID(), userKeyHandler.Create)
	users.Get("/:user_id/api-keys", middleware.RequireUserID(), userKeyHandler.List)
	users.Delete("/:user_id/api-keys/:key_id", middleware.RequireUserID(), userKeyHandler.Revoke)

	api := app.Group("/api/organizations")
	api.Post("/", middleware.RequireUserID(), orgHandler.CreateOrganization)
	api.Get("/", middleware.RequireUserID(), orgHandler.ListOrganizations)
	api.Get("/:organization_id", middleware.RequireUserID(), orgHandler.GetOrganization)
	api.Patch("/:organization_id", middleware.RequireUserID(), orgHandler.UpdateOrganization)
	api.Delete("/:organization_id", middleware.RequireUserID(), orgHandler.DeleteOrganization)

	api.Get("/:organization_id/members", middleware.RequireUserID(), orgHandler.ListMembers)
	api.Post("/:organization_id/members", middleware.RequireUserID(), orgHandler.CreateMember)
	api.Get("/:organization_id/members/:member_id", middleware.RequireUserID(), orgHandler.GetMember)
	api.Patch("/:organization_id/members/:member_id", middleware.RequireUserID(), orgHandler.UpdateMember)
	api.Delete("/:organization_id/members/:member_id", middleware.RequireUserID(), orgHandler.DeleteMember)

	api.Post("/:organization_id/members/:member_id/api-keys", middleware.RequireUserID(), memberKeyHandler.Create)
	api.Get("/:organization_id/members/:member_id/api-keys", middleware.RequireUserID(), memberKeyHandler.List)
	api.Delete("/:organization_id/members/:member_id/api-keys/:key_id", middleware.RequireUserID(), memberKeyHandler.Revoke)

	api.Post("/:organization_id/service-accounts", middleware.RequireUserID(), orgHandler.CreateServiceAccount)
	api.Get("/:organization_id/service-accounts", middleware.RequireUserID(), orgHandler.ListServiceAccounts)
	api.Get("/:organization_id/service-accounts/:service_account_id", middleware.RequireUserID(), orgHandler.GetServiceAccount)
	api.Patch("/:organization_id/service-accounts/:service_account_id", middleware.RequireUserID(), orgHandler.UpdateServiceAccount)
	api.Delete("/:organization_id/service-accounts/:service_account_id", middleware.RequireUserID(), orgHandler.DeleteServiceAccount)

	port := os.Getenv("PORT")
	if port == "" {
		port = "3000"
	}

	internalApp := fiber.New(fiber.Config{
		AppName:      "identity-internal",
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

	internalApp.Post("/internal/members/resolve", middleware.RequireInternalToken(), orgHandler.Resolve)
	internalApp.Post("/internal/user-keys/validate", middleware.RequireInternalToken(), userKeyHandler.Validate)
	internalApp.Post("/internal/member-keys/validate", middleware.RequireInternalToken(), memberKeyHandler.Validate)

	internalPort := os.Getenv("INTERNAL_PORT")
	if internalPort == "" {
		internalPort = "3001"
	}

	baseLogger := telem.Logger()
	baseLogger.Info().
		Str("port", port).
		Str("internal_port", internalPort).
		Msg("starting identity server")

	go func() {
		if err := internalApp.Listen(":" + internalPort); err != nil {
			baseLogger.Fatal().Err(err).Msg("internal server exited")
		}
	}()

	if err := app.Listen(":" + port); err != nil {
		baseLogger.Fatal().Err(err).Msg("fiber server exited")
	}
}
