COMPOSE = docker compose -f deploy/docker-compose.yml --env-file .env

.PHONY: build test test-db test-e2e lint run dev images up up-heavy down logs stats status backup

build:            ## build the binary into ./bin
	go build -o bin/mcpkg ./cmd/mcpkg

test:             ## unit tests (no database, no network)
	go test ./...

test-db:          ## + Postgres integration tests (export TEST_DATABASE_URL first)
	go test ./internal/e2e -run 'TestQueueSemantics|TestDiscoverDedupe' -v

test-e2e:         ## full pipeline with real npm/PyPI installs (needs TEST_DATABASE_URL, npm, uv)
	E2E_NETWORK=1 go test ./internal/e2e -run TestFullPipeline -v -timeout 15m

lint:
	gofmt -l . && go vet ./...

dev:              ## local Postgres + web on :8080 (see scripts/dev.sh)
	./scripts/dev.sh

images:           ## app image, sandbox runner image, egress proxy image
	$(COMPOSE) build
	docker build -f sandbox/runner.Dockerfile -t mcpkg-runner:latest sandbox

up:               ## core services (db, web, scheduler, light worker, caddy)
	$(COMPOSE) up -d

up-heavy:         ## core + heavy worker (security + smoke) on the same box
	$(COMPOSE) --profile heavy up -d

down:
	$(COMPOSE) --profile heavy down

logs:
	$(COMPOSE) logs -f --tail=100

stats:
	$(COMPOSE) run --rm --no-deps web stats

status:
	./scripts/status.sh

backup:
	./scripts/backup.sh
