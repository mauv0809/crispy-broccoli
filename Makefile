.PHONY: build run dev db-up db-down migrate-create templ-generate templ-watch css-build css-watch swagger tools setup

# Build everything
build: templ-generate css-build swagger
	go build -o bin/app ./cmd/app

# Run the application (migrations run automatically on startup)
run: templ-generate css-build swagger
	go run ./cmd/app

# Run with live reload (requires air: go install github.com/air-verse/air@latest)
dev:
	air

# Start database
db-up:
	docker compose up -d

# Stop database
db-down:
	docker compose down

# Create a new migration file
migrate-create:
	@read -p "Migration name: " name; \
	goose -dir internal/db/migrations create $$name sql

# Generate templ files
templ-generate:
	templ generate

# Watch templ files for changes
templ-watch:
	templ generate --watch

# Build CSS
css-build:
	npm run css:build

# Watch CSS for changes
css-watch:
	npm run css:watch

# Generate swagger docs
swagger:
	swag init -g cmd/app/main.go --parseDependency --parseInternal

# Install dev tools
tools:
	go install github.com/a-h/templ/cmd/templ@latest
	go install github.com/pressly/goose/v3/cmd/goose@latest
	go install github.com/air-verse/air@latest
	go install github.com/swaggo/swag/cmd/swag@latest

# First-time setup
setup:
	npm install
	make tools
