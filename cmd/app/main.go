package main

import (
	"context"
	"log"
	"os"

	"github.com/joho/godotenv"
	"github.com/labstack/echo/v4"
	"github.com/labstack/echo/v4/middleware"
	"github.com/mauv0809/crispy-broccoli/internal/db"
	"github.com/mauv0809/crispy-broccoli/internal/handlers"
	"github.com/mauv0809/crispy-broccoli/internal/ingest"
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
	// Load .env file if it exists (local dev)
	if err := godotenv.Load(); err != nil {
		log.Println("No .env file found, using environment variables")
	}

	ctx := context.Background()

	// Get database URL
	databaseURL := os.Getenv("DATABASE_URL")
	if databaseURL == "" {
		log.Fatal("DATABASE_URL environment variable is required")
	}

	// Run migrations
	if err := db.RunMigrations(databaseURL); err != nil {
		log.Printf("Warning: Could not run migrations: %v", err)
	} else {
		log.Println("Migrations completed")
	}

	// Connect to database
	pool, err := db.Connect(ctx, databaseURL)
	if err != nil {
		log.Printf("Warning: Could not connect to database: %v", err)
		log.Println("Continuing without database connection...")
	} else {
		defer pool.Close()
		log.Println("Connected to database")
	}

	// Setup Echo
	e := echo.New()
	e.Use(middleware.RequestLoggerWithConfig(middleware.RequestLoggerConfig{
		LogStatus:   true,
		LogURI:      true,
		LogError:    true,
		HandleError: true,
		LogValuesFunc: func(c echo.Context, v middleware.RequestLoggerValues) error {
			if v.Error == nil {
				log.Printf("%d %s", v.Status, v.URI)
			} else {
				log.Printf("%d %s - %v", v.Status, v.URI, v.Error)
			}
			return nil
		},
	}))
	e.Use(middleware.Recover())

	// Setup handlers
	h := handlers.New(pool)

	// Setup repository and ingest client (if database is available)
	var ingestHandler *handlers.IngestHandler
	var strategyHandler *handlers.StrategyHandler
	if pool != nil {
		repo := db.NewRepository(pool)

		// Setup Nasdaq Data Link client (for SEP equity prices and fundamentals)
		var nasdaqClient *ingest.Client
		nasdaqAPIKey := os.Getenv("NASDAQ_API_KEY")
		if nasdaqAPIKey != "" {
			nasdaqClient = ingest.NewClient(nasdaqAPIKey)
			log.Println("Nasdaq Data Link client initialized (SEP for equity prices)")
		} else {
			log.Println("Warning: NASDAQ_API_KEY not set, Nasdaq data endpoints disabled")
		}

		// Setup Tiingo client (for ETF benchmark prices - SPY, QQQ, etc.)
		var tiingoClient *ingest.TiingoClient
		tiingoAPIKey := os.Getenv("TIINGO_API_KEY")
		if tiingoAPIKey != "" {
			tiingoClient = ingest.NewTiingoClient(tiingoAPIKey)
			log.Println("Tiingo client initialized (for ETF benchmarks and stock prices)")
		} else {
			log.Println("Warning: TIINGO_API_KEY not set, ETF benchmark comparison disabled")
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
		log.Println("Strategy engine initialized")

		// Seed default strategies
		if err := strategy.SeedDefaultStrategies(ctx, pool); err != nil {
			log.Printf("Warning: failed to seed default strategies: %v", err)
		}
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
		log.Println("Strategy endpoints registered")
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
		log.Println("Ingestion endpoints registered")
	}

	// Start server
	port := os.Getenv("PORT")
	if port == "" {
		port = "8080"
	}

	log.Printf("Starting server on :%s", port)
	if err := e.Start(":" + port); err != nil {
		log.Fatalf("Failed to start server: %v", err)
	}
}
