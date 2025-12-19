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
)

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
	h := handlers.New()

	// Setup repository and ingest client (if database is available)
	var ingestHandler *handlers.IngestHandler
	if pool != nil {
		repo := db.NewRepository(pool)

		// Setup ingest client (requires NASDAQ_API_KEY)
		nasdaqAPIKey := os.Getenv("NASDAQ_API_KEY")
		if nasdaqAPIKey != "" {
			ingestClient := ingest.NewClient(nasdaqAPIKey)
			ingestHandler = handlers.NewIngestHandler(ingestClient, repo)
			log.Println("Ingest client initialized")
		} else {
			log.Println("Warning: NASDAQ_API_KEY not set, ingestion endpoints disabled")
		}
	}

	// Static files
	e.Static("/assets", "assets")

	// Routes
	e.GET("/health", h.Health)
	e.GET("/", h.Index)

	// Admin routes for data ingestion
	if ingestHandler != nil {
		admin := e.Group("/admin")
		admin.GET("/ingest/status", ingestHandler.IngestStatus)
		admin.GET("/ingest/test", ingestHandler.IngestTest)
		admin.POST("/ingest/tickers", ingestHandler.IngestTickers)
		admin.POST("/ingest/fundamentals", ingestHandler.IngestFundamentals)
		admin.POST("/ingest/daily", ingestHandler.IngestDaily)
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
