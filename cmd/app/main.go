package main

import (
	"context"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/joho/godotenv"
	"github.com/labstack/echo/v4"
	"github.com/labstack/echo/v4/middleware"
	"github.com/mauv0809/crispy-broccoli/internal/db"
	"github.com/mauv0809/crispy-broccoli/internal/handlers"
	"github.com/mauv0809/crispy-broccoli/internal/ingest"
	"github.com/mauv0809/crispy-broccoli/internal/observability"
	"github.com/mauv0809/crispy-broccoli/internal/strategy"

	"github.com/mauv0809/crispy-broccoli/docs"
)

// Build metadata, populated via -ldflags at build time.
// See Dockerfile for the build args wiring.
var (
	buildSHA  = "dev"
	buildTime = "unknown"
)

// @title DeepValue API
// @version 1.0
// @description Personal value investing portfolio manager API
// @host localhost:8080
// @BasePath /

func main() {
	env := os.Getenv("ENV")
	if env == "" {
		env = "development"
	}
	logger := observability.NewLogger(observability.Config{
		Env:   env,
		Level: observability.ParseLevel(os.Getenv("LOG_LEVEL")),
	})
	slog.SetDefault(logger)

	// Load .env file if it exists (local dev)
	if err := godotenv.Load(); err != nil {
		slog.Info("no .env file found, using environment variables")
	}

	ctx := context.Background()

	// Get database URL
	databaseURL := os.Getenv("DATABASE_URL")
	if databaseURL == "" {
		slog.Error("DATABASE_URL environment variable is required")
		os.Exit(1)
	}

	// Run migrations. Fail fast on error: an HTTP server with a broken
	// or stale schema is worse than a restart loop.
	if err := db.RunMigrations(databaseURL); err != nil {
		slog.Error("migrations failed", "error", err)
		os.Exit(1)
	}
	slog.Info("migrations completed")

	// Connect to database. Fail fast: same reasoning as migrations.
	pool, err := db.Connect(ctx, databaseURL)
	if err != nil {
		slog.Error("database connect failed", "error", err)
		os.Exit(1)
	}
	defer pool.Close()
	slog.Info("database connected")

	// Setup Echo
	e := echo.New()
	e.HideBanner = true
	e.HidePort = true
	e.Use(observability.RequestIDMiddleware())
	e.Use(observability.RequestLoggerMiddleware(logger))
	e.Use(middleware.Recover())

	// Setup handlers
	handlers.SetBuildInfo(buildSHA, buildTime)
	h := handlers.New(pool)

	// Setup repository and ingest client
	var ingestHandler *handlers.IngestHandler
	var strategyHandler *handlers.StrategyHandler
	repo := db.NewRepository(pool)

	// Setup Nasdaq Data Link client (for SEP equity prices and fundamentals)
	var nasdaqClient *ingest.Client
	nasdaqAPIKey := os.Getenv("NASDAQ_API_KEY")
	if nasdaqAPIKey != "" {
		nasdaqClient = ingest.NewClient(nasdaqAPIKey)
		slog.Info("nasdaq client initialized")
	} else {
		slog.Warn("NASDAQ_API_KEY not set; nasdaq endpoints disabled")
	}

	// Setup Tiingo client (for ETF benchmark prices - SPY, QQQ, etc.)
	var tiingoClient *ingest.TiingoClient
	tiingoAPIKey := os.Getenv("TIINGO_API_KEY")
	if tiingoAPIKey != "" {
		tiingoClient = ingest.NewTiingoClient(tiingoAPIKey)
		slog.Info("tiingo client initialized")
	} else {
		slog.Warn("TIINGO_API_KEY not set; benchmark comparison disabled")
	}

	// Setup ingest handler (needs both clients)
	if nasdaqClient != nil {
		ingestHandler = handlers.NewIngestHandler(nasdaqClient, tiingoClient, repo)
	}

	// Setup strategy handler with backtester
	strategyRepo := strategy.NewRepository(pool)
	strategyExecutor := strategy.NewExecutor(pool)
	backtester := strategy.NewBacktester(strategyExecutor, repo, nasdaqClient, tiingoClient)
	strategyHandler = handlers.NewStrategyHandler(strategyRepo, strategyExecutor, backtester)
	slog.Info("strategy engine initialized")

	// Seed default strategies
	if err := strategy.SeedDefaultStrategies(ctx, pool); err != nil {
		slog.Warn("failed to seed default strategies", "error", err)
	}

	// Static files
	e.Static("/assets", "assets")

	// Routes
	e.GET("/health", h.Health)
	e.GET("/", h.Index)
	e.GET("/docs", h.Docs)

	// Serve OpenAPI spec directly
	e.GET("/api/openapi.json", func(c echo.Context) error {
		return c.JSONBlob(200, []byte(docs.SwaggerInfo.ReadDoc()))
	})

	// Strategy API routes
	if strategyHandler != nil {
		// JSON API endpoints
		api := e.Group("/api")
		api.GET("/strategies", strategyHandler.ListStrategies)
		api.POST("/strategies", strategyHandler.CreateStrategy)
		api.POST("/strategies/preview", strategyHandler.PreviewStrategy)
		api.GET("/strategies/:id", strategyHandler.GetStrategy)
		api.PUT("/strategies/:id", strategyHandler.UpdateStrategy)
		api.DELETE("/strategies/:id", strategyHandler.DeleteStrategy)
		api.POST("/strategies/:id/run", strategyHandler.RunStrategyHTMX) // Returns HTML for HTMX
		api.GET("/strategies/:id/runs", strategyHandler.GetStrategyRunsHTMX) // Returns HTML for HTMX
		api.GET("/strategies/:id/stats", strategyHandler.GetStrategyStats)
		api.POST("/strategies/:id/backtest", strategyHandler.RunBacktest)          // JSON API for backtest
		api.POST("/strategies/:id/backtest-htmx", strategyHandler.RunBacktestHTMX) // HTML for HTMX
		api.GET("/strategy-fields", strategyHandler.GetStrategyFields)

		// HTML Page routes
		e.GET("/strategies", strategyHandler.StrategiesPage)
		e.GET("/strategies/new", strategyHandler.NewStrategyPage)
		e.GET("/strategies/:id", strategyHandler.StrategyDetailPage)
		e.GET("/strategies/:id/edit", strategyHandler.EditStrategyPage)

		// Dashboard API (returns HTML fragments for HTMX)
		api.GET("/dashboard/strategies", strategyHandler.DashboardStrategies)
		api.GET("/dashboard/runs", strategyHandler.DashboardRuns)
		slog.Info("strategy endpoints registered")
	}

	// Admin routes for data ingestion (Sharadar - fundamentals only)
	if ingestHandler != nil {
		admin := e.Group("/admin")
		admin.GET("/ingest/status", ingestHandler.IngestStatus)
		admin.POST("/ingest/tickers", ingestHandler.IngestTickers)
		admin.POST("/ingest/fundamentals", ingestHandler.IngestFundamentals)
		admin.POST("/ingest/sp500", ingestHandler.IngestSP500)
		admin.POST("/ingest/benchmark", ingestHandler.IngestBenchmark)
		admin.POST("/ingest/prices", ingestHandler.IngestPrices)
		slog.Info("ingestion endpoints registered")
	}

	// Start server
	port := os.Getenv("PORT")
	if port == "" {
		port = "8080"
	}

	// Start server in a goroutine so we can listen for shutdown signals.
	go func() {
		slog.Info("starting server", "port", port)
		if err := e.Start(":" + port); err != nil && err != http.ErrServerClosed {
			slog.Error("server error", "error", err)
			os.Exit(1)
		}
	}()

	// Wait for SIGINT or SIGTERM. Coolify sends SIGTERM on redeploy.
	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM)
	<-sigCh
	slog.Info("shutdown signal received; draining")

	shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := e.Shutdown(shutdownCtx); err != nil {
		slog.Error("graceful shutdown error", "error", err)
	}
	slog.Info("server stopped")
}
