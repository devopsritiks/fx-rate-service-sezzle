BINARY := bin/fxservice
FAKEVENDOR_BINARY := bin/fakevendor
PKG := ./...
PORT := 8080
FAKEVENDOR_PORT := 9090

.PHONY: run build test lint fmt clean stop port-check fakevendor-build fakevendor-run fakevendor-stop \
        up up-build down logs chaos-on chaos-off db-shell audit-tail db-down db-up

POSTGRES_USER ?= fx
POSTGRES_DB ?= fx

# Checks that PORT is free before starting the server. Without this, a
# server left running from a previous session silently keeps serving
# stale config/state while you think you're hitting a fresh one — exactly
# what happened during the phase 2 demo. Fails loudly instead of letting
# you debug the wrong process for ten minutes.
port-check:
	@if lsof -ti:$(PORT) >/dev/null 2>&1; then \
		echo "ERROR: port $(PORT) is already in use (pid $$(lsof -ti:$(PORT)))."; \
		echo "Run 'make stop' to kill it, or inspect with: lsof -i:$(PORT)"; \
		exit 1; \
	fi

# Kills whatever is listening on PORT. Safe to run even if nothing is
# listening (lsof -ti finds nothing, xargs with -r is a no-op).
stop:
	@lsof -ti:$(PORT) | xargs -r kill -9
	@echo "port $(PORT) is now free"

run: port-check
	go run ./cmd/fxservice

build:
	go build -o $(BINARY) ./cmd/fxservice

test:
	go test -race -cover $(PKG)

fmt:
	gofmt -w .

lint:
	@if command -v golangci-lint >/dev/null 2>&1; then \
		golangci-lint run $(PKG); \
	else \
		echo "golangci-lint not installed; falling back to go vet + gofmt check"; \
		echo "install it with: brew install golangci-lint"; \
		gofmt -l . ; \
		test -z "$$(gofmt -l .)" ; \
		go vet $(PKG); \
	fi

clean:
	rm -rf bin

# --- fakevendor: toggleable proxy for live chaos/stale-cache demos ---

fakevendor-build:
	go build -o $(FAKEVENDOR_BINARY) ./cmd/fakevendor

fakevendor-run: fakevendor-build
	@if lsof -ti:$(FAKEVENDOR_PORT) >/dev/null 2>&1; then \
		echo "ERROR: port $(FAKEVENDOR_PORT) is already in use (pid $$(lsof -ti:$(FAKEVENDOR_PORT)))."; \
		echo "Run 'make fakevendor-stop' first."; \
		exit 1; \
	fi
	./$(FAKEVENDOR_BINARY)

fakevendor-stop:
	@lsof -ti:$(FAKEVENDOR_PORT) | xargs -r kill -9
	@echo "port $(FAKEVENDOR_PORT) is now free"

# --- full stack: fxservice + fakevendor + postgres + prometheus + grafana ---

# Default path: pull pre-built images from GHCR, no Go toolchain and no
# local Docker build needed at all. This is the "one command" entry
# point — see README.
up:
	docker compose up -d
	@echo ""
	@echo "fxservice:  http://localhost:8080   (UI + API)"
	@echo "fakevendor: http://localhost:9090   (POST /admin/mode?mode=off|error|slow)"
	@echo "prometheus: http://localhost:9091"
	@echo "grafana:    http://localhost:3000   (anonymous viewer, dashboard pre-loaded)"

# Build fxservice/fakevendor from local source instead of pulling from
# GHCR — for when you're actively changing the Go code. Everything else
# (postgres/prometheus/grafana) is still pulled either way.
up-build:
	docker compose -f docker-compose.yml -f docker-compose.build.yml up -d --build
	@echo ""
	@echo "fxservice:  http://localhost:8080   (UI + API, built from local source)"
	@echo "fakevendor: http://localhost:9090   (POST /admin/mode?mode=off|error|slow)"
	@echo "prometheus: http://localhost:9091"
	@echo "grafana:    http://localhost:3000   (anonymous viewer, dashboard pre-loaded)"

down:
	docker compose down -v

logs:
	docker compose logs -f

# Toggle fakevendor's failure injection from outside the stack, so you
# can watch the circuit breaker trip and Grafana's graphs react without
# restarting fxservice or losing its cache. mode=error fails immediately;
# see README for mode=slow.
chaos-on:
	curl -s -X POST "http://localhost:9090/admin/mode?mode=error" && echo

chaos-off:
	curl -s -X POST "http://localhost:9090/admin/mode?mode=off" && echo

# --- postgres / audit log demo targets ---

# Opens a psql shell inside the running postgres container.
db-shell:
	docker compose exec postgres psql -U $(POSTGRES_USER) -d $(POSTGRES_DB)

# Shows the 10 most recent audit_log rows — the quickest way to confirm
# requests are actually landing in Postgres.
audit-tail:
	docker compose exec postgres psql -U $(POSTGRES_USER) -d $(POSTGRES_DB) -c \
		"SELECT id, request_id, occurred_at, endpoint, from_currency, to_currency, amount, rate, cache_status, stale, http_status, error_code, latency_ms FROM audit_log ORDER BY occurred_at DESC LIMIT 10;"

# Stops (but does not remove) the postgres container, simulating a
# database outage while the rest of the stack — including fxservice —
# keeps running. Pair with a few curl calls and watch fxservice_audit_*
# metrics in Grafana: queue depth climbs, write errors increment, the API
# itself keeps returning 200s the whole time.
db-down:
	docker compose stop postgres

# Brings postgres back. The audit writer's own retry/backoff picks the
# queued-and-not-yet-dropped records back up automatically — no restart
# of fxservice needed.
db-up:
	docker compose start postgres
