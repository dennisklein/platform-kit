# platform-kit: build, test and run the demo.
# Go uses the local toolchain (go.mod says go 1.24.3); no toolchain download.
#
# Demo (Docker only):  make up | make reset | make fresh | make down | make status
# Development:         make gen | make web | make build | make test | make run | make web-dev

export GOTOOLCHAIN := local
export CGO_ENABLED ?= 0

VERSION ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)
LDFLAGS := -s -w -X main.version=$(VERSION)
BIN     := bin/pk
DATA    ?= $(CURDIR)/.data
IMAGE   ?= platform-kit:dev
# make run: the bench from source on this port (the compose bench keeps 8080)
RUN_PORT ?= 8090
FORGE_PORT := $(or $(PK_FORGE_PORT),3000)
FLUX_PORT  := $(or $(PK_FLUX_PORT),8081)

.PHONY: help all gen gen-go gen-web build web web-deps web-dev test test-go test-web lint vet run \
	docker up down reset fresh preflight status logs llm-check e2e clean

## help: list the targets
help:
	@sed -n 's/^## //p' $(MAKEFILE_LIST)

all: gen web build

## gen: make the Go server, Go client and TS types from api/openapi.yaml
gen: gen-go gen-web

gen-go:
	go generate ./internal/api/... ./pkg/client/...

gen-web: web-deps
	cd web && npm run gen

web-deps: web/node_modules/.package-lock.json

web/node_modules/.package-lock.json: web/package.json web/package-lock.json
	cd web && npm ci
	@touch $@

## web: build the SPA into web/dist (embedded by the Go binary)
web: web-deps
	cd web && npm run build

## build: build bin/pk (embeds web/dist as it is; run make web first)
build:
	go build -trimpath -ldflags '$(LDFLAGS)' -o $(BIN) ./cmd/pk

## test: Go tests, web unit tests and the svelte type check
test: test-go test-web

test-go:
	go test ./...

test-web: web-deps
	cd web && npm run test:unit && npm run check

## lint: go vet
lint: vet

vet:
	go vet ./...

## run: the bench from source on 127.0.0.1:$(RUN_PORT), with Gitea, ci and fluxsim from compose
run: build
	mkdir -p $(DATA)
	docker compose up -d gitea forge-init ci fluxsim
	docker compose cp forge-init:/creds/forge.json $(DATA)/forge.json
	PK_DEMO=1 PK_DATA=$(DATA) PK_ADDR=127.0.0.1:$(RUN_PORT) \
		PK_ALLOWED_HOSTS=localhost:5173,127.0.0.1:5173 \
		PK_FORGE_URL=http://127.0.0.1:$(FORGE_PORT) PK_FORGE_CREDENTIALS=$(DATA)/forge.json \
		PK_FLUX_URL=http://127.0.0.1:$(FLUX_PORT) \
		$(BIN) bench

## web-dev: Vite dev server on http://localhost:5173 with hot reload, against the bench of make run
web-dev: web-deps
	cd web && PK_BENCH_URL=$${PK_BENCH_URL:-http://127.0.0.1:$(RUN_PORT)} npm run dev

## docker: build the image (multi-arch: docker buildx build --platform linux/amd64,linux/arm64 .)
docker:
	docker build --build-arg VERSION=$(VERSION) -t $(IMAGE) .

## up: build the image and start the demo stack (http://127.0.0.1:8080); checks the ports first
up: preflight
	docker compose up -d --build --wait

## down: stop the demo stack; keep its data
down:
	docker compose down

## reset: rehearsal reset: rebuild the forge state (seed history and open changes); the stack keeps running
reset:
	docker compose run --rm forge-init forge-init --reset

## fresh: delete all demo data (forge, bench, LLM log, agent tokens) and start again
fresh:
	docker compose down -v
	./scripts/preflight-ports.sh
	docker compose up -d --wait

## preflight: check that the host ports of bench, Gitea and fluxsim are free (PK_*_PORT, .env)
preflight:
	./scripts/preflight-ports.sh

## status: containers, and bench status (forge, LLM, Flux, main)
status:
	docker compose ps
	docker compose exec -T bench pk me get-status

## logs: follow the logs of bench, ci and fluxsim
logs:
	docker compose logs -f --tail 50 bench ci fluxsim

## llm-check: test the LLM endpoint from inside the bench container (PK_LLM_BASE_URL)
llm-check:
	./scripts/llm-check.sh

## e2e: the demo flow through the API against docker compose, then the UI smoke test
e2e:
	./scripts/e2e.sh

clean:
	rm -rf bin $(DATA)
	find web/dist -mindepth 1 ! -name .gitkeep -exec rm -rf {} +
