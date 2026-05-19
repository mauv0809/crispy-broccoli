.PHONY: build run dev db-up db-down migrate-create templ-generate templ-watch css-build css-watch swagger test tools setup

# Build everything
build: templ-generate css-build swagger
	go build -o bin/app ./cmd/app

# Run all tests serially across packages.
#
# `-p 1` is required because testutil.OpenTestDB shares one Postgres database
# across every test package. Parallel package execution races on the
# TRUNCATE+INSERT-system-user fixture and on cross-package writes to shared
# tables (strategies, portfolios). Per-package isolation is a future option
# (separate schemas) but not worth the complexity today.
test:
	go test -p 1 ./...

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

# Install dev tools at the versions pinned in go.mod (templ, swag) plus
# floaters that aren't imported anywhere (goose, air). Bump pinned
# versions by editing tools.go + running `go get`.
tools:
	go install github.com/a-h/templ/cmd/templ
	go install github.com/swaggo/swag/cmd/swag
	go install github.com/pressly/goose/v3/cmd/goose@latest
	go install github.com/air-verse/air@latest

# First-time setup
setup:
	npm install
	make tools
