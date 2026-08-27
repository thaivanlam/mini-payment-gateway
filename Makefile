.PHONY: up down logs migrate migrate-down run worker test test-int test-cover lint seed build docker-build reconcile tidy

COVERAGE_THRESHOLD ?= 70

up: ## start postgres + redis + api + worker
	docker compose up -d --build

down:
	docker compose down -v

logs:
	docker compose logs -f api worker

migrate: ## apply goose migrations
	go run ./cmd/migrate up

migrate-down:
	go run ./cmd/migrate down

run: ## run the API locally
	go run ./cmd/api

worker: ## run the webhook worker locally
	go run ./cmd/worker

seed: ## create a demo merchant + sample transactions
	go run ./cmd/seed

reconcile: ## run the reconciliation job for DATE (default: today)
	go run ./cmd/worker -job=reconcile -date=$(or $(DATE),$(shell date +%F))

test: ## unit tests with race detector
	go test ./... -race -cover

test-int: ## integration tests (needs postgres + redis from docker compose)
	go test ./test/integration/... -tags=integration -race -count=1 -v

test-cover: ## combined unit + integration coverage (needs `make up`)
	go test ./... ./test/integration/... -tags=integration -count=1 		-coverpkg=./internal/... -coverprofile=coverage.out -covermode=atomic
	go tool cover -func=coverage.out | tail -1

lint:
	golangci-lint run ./...

build:
	CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o bin/api ./cmd/api
	CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o bin/worker ./cmd/worker

docker-build:
	docker build -t mini-payment-gateway:local .

tidy:
	go mod tidy
