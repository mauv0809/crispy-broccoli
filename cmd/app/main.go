package main

import (
	"context"
	"database/sql"
	"encoding/hex"
	"flag"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/joho/godotenv"
	"github.com/labstack/echo/v4"
	"github.com/labstack/echo/v4/middleware"
	"github.com/markbates/goth/gothic"

	"github.com/mauv0809/crispy-broccoli/internal/auth"
	"github.com/mauv0809/crispy-broccoli/internal/buildinfo"
	"github.com/mauv0809/crispy-broccoli/internal/db"
	"github.com/mauv0809/crispy-broccoli/internal/handlers"
	"github.com/mauv0809/crispy-broccoli/internal/ingest"
	"github.com/mauv0809/crispy-broccoli/internal/observability"
	"github.com/mauv0809/crispy-broccoli/internal/strategy"
	"github.com/mauv0809/crispy-broccoli/internal/users"

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
	addUserEmail := flag.String("add-user", "", "Add a user with the given email and exit")
	disableUserEmail := flag.String("disable-user", "", "Disable the user with the given email and exit")
	addUserAdmin := flag.Bool("admin", false, "When used with --add-user, mark the user as an admin")
	flag.Parse()

	env := os.Getenv("ENV")
	if env == "" {
		env = "development"
	}
	logger := observability.NewLogger(observability.Config{
		Env:   env,
		Level: observability.ParseLevel(os.Getenv("LOG_LEVEL")),
	})
	slog.SetDefault(logger)

	sentryCleanup, sentryEnabled := observability.InitSentry(
		os.Getenv("SENTRY_DSN"),
		env,
		buildSHA,
	)
	defer sentryCleanup()

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

	usersRepo := users.NewRepository(pool)

	// CLI short-circuit: provisioning commands don't need to start the HTTP server.
	if *addUserEmail != "" {
		if err := users.AddUser(ctx, usersRepo, *addUserEmail, *addUserAdmin); err != nil {
			slog.Error("add-user failed", "error", err)
			os.Exit(1)
		}
		return
	}
	if *disableUserEmail != "" {
		if err := users.DisableUser(ctx, usersRepo, *disableUserEmail); err != nil {
			slog.Error("disable-user failed", "error", err)
			os.Exit(1)
		}
		return
	}

	// Sessions: scs uses database/sql, not pgx. Open a small companion pool.
	sqlDB, err := sql.Open("pgx", databaseURL)
	if err != nil {
		slog.Error("session db open failed", "error", err)
		os.Exit(1)
	}
	defer sqlDB.Close()
	sessionManager := auth.NewSessionManager(sqlDB)
	sessionManager.Cookie.Secure = env == "production"

	// Gothic store (for the short-lived OAuth state cookie).
	sessionKeyHex := os.Getenv("SESSION_KEY")
	if env == "production" && sessionKeyHex == "" {
		slog.Error("SESSION_KEY required in production")
		os.Exit(1)
	}
	sessionKey, err := hex.DecodeString(sessionKeyHex)
	if err != nil || len(sessionKey) < 32 {
		if env == "production" {
			slog.Error("SESSION_KEY must be hex-encoded and at least 32 bytes")
			os.Exit(1)
		}
		// dev fallback: stable per-process random key
		sessionKey = []byte("dev-session-key-not-for-production-use")
	}
	gothic.Store = auth.NewGothicStore(sessionKey, env == "production")

	// Google OAuth provider.
	if cid, csec := os.Getenv("GOOGLE_CLIENT_ID"), os.Getenv("GOOGLE_CLIENT_SECRET"); cid != "" && csec != "" {
		if err := auth.RegisterGoogle(auth.GoogleConfig{
			ClientID:     cid,
			ClientSecret: csec,
			BaseURL:      os.Getenv("BASE_URL"),
		}); err != nil {
			slog.Error("google oauth init failed", "error", err)
			os.Exit(1)
		}
		slog.Info("google oauth provider registered")
	} else {
		slog.Warn("google oauth disabled; GOOGLE_CLIENT_ID/SECRET not set")
	}

	googleHandler := auth.NewGoogleHandler(sessionManager, usersRepo)
	authMiddleware := auth.RequireAuth(auth.NewSession(sessionManager), auth.NewLoader(usersRepo))
	adminMiddleware := auth.RequireAdmin()

	// Setup Echo
	e := echo.New()
	e.HideBanner = true
	e.HidePort = true
	e.Use(observability.RequestIDMiddleware())
	e.Use(observability.RequestLoggerMiddleware(logger))
	e.Use(middleware.Secure())
	e.Use(middleware.BodyLimit("1M"))
	e.Use(observability.SentryErrorMiddleware(sentryEnabled))
	e.Use(middleware.Recover())
	e.Use(auth.SessionMiddleware(sessionManager))
	e.Use(middleware.CSRFWithConfig(middleware.CSRFConfig{
		TokenLookup:    "header:X-CSRF-Token,form:_csrf",
		CookieName:     "_csrf",
		CookiePath:     "/",
		CookieHTTPOnly: false, // JS needs to read it indirectly via meta tag
		CookieSameSite: http.SameSiteLaxMode,
		CookieSecure:   env == "production",
		Skipper: func(c echo.Context) bool {
			p := c.Request().URL.Path
			switch {
			case p == "/health",
				p == "/api/openapi.json",
				strings.HasPrefix(p, "/assets/"),
				strings.HasPrefix(p, "/auth/"):
				return true
			}
			return false
		},
	}))

	// Setup handlers
	h := handlers.New(pool, buildinfo.Info{SHA: buildSHA, Time: buildTime})

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
	googleHandler.Mount(e)

	// Routes
	e.GET("/health", h.Health)
	e.GET("/", h.Index, authMiddleware)
	e.GET("/docs", h.Docs, authMiddleware)

	// Serve OpenAPI spec directly
	e.GET("/api/openapi.json", func(c echo.Context) error {
		return c.JSONBlob(http.StatusOK, []byte(docs.SwaggerInfo.ReadDoc()))
	})

	// Strategy API routes
	if strategyHandler != nil {
		// JSON API endpoints
		api := e.Group("/api", authMiddleware)
		api.GET("/strategies", strategyHandler.ListStrategies)
		api.POST("/strategies", strategyHandler.CreateStrategy)
		api.POST("/strategies/preview", strategyHandler.PreviewStrategy)
		api.GET("/strategies/:id", strategyHandler.GetStrategy)
		api.PUT("/strategies/:id", strategyHandler.UpdateStrategy)
		api.DELETE("/strategies/:id", strategyHandler.DeleteStrategy)
		api.POST("/strategies/:id/run", strategyHandler.RunStrategyHTMX)     // Returns HTML for HTMX
		api.GET("/strategies/:id/runs", strategyHandler.GetStrategyRunsHTMX) // Returns HTML for HTMX
		api.GET("/strategies/:id/stats", strategyHandler.GetStrategyStats)
		api.POST("/strategies/:id/backtest", strategyHandler.RunBacktest)          // JSON API for backtest
		api.POST("/strategies/:id/backtest-htmx", strategyHandler.RunBacktestHTMX) // HTML for HTMX
		api.GET("/strategy-fields", strategyHandler.GetStrategyFields)

		// HTML Page routes
		e.GET("/strategies", strategyHandler.StrategiesPage, authMiddleware)
		e.GET("/strategies/new", strategyHandler.NewStrategyPage, authMiddleware)
		e.GET("/strategies/:id", strategyHandler.StrategyDetailPage, authMiddleware)
		e.GET("/strategies/:id/edit", strategyHandler.EditStrategyPage, authMiddleware)

		// Dashboard API (returns HTML fragments for HTMX)
		api.GET("/dashboard/strategies", strategyHandler.DashboardStrategies)
		api.GET("/dashboard/runs", strategyHandler.DashboardRuns)
		slog.Info("strategy endpoints registered")
	}

	// Admin routes for data ingestion (Sharadar - fundamentals only)
	if ingestHandler != nil {
		admin := e.Group("/admin", authMiddleware, adminMiddleware)
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
