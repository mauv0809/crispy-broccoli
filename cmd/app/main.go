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

	// Static files
	e.Static("/assets", "assets")

	// Routes
	e.GET("/health", h.Health)
	e.GET("/", h.Index)

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
