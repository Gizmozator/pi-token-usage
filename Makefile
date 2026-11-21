IMAGE    ?= pi-token-usage:latest
NAME     ?= pitokens
SESSIONS ?= $(HOME)/.pi/agent/sessions
OPENCODE_DATA ?= $(HOME)/.local/share/opencode
OCR_HOME ?= $(HOME)/.opencodereview
# Run as the host user so the container can read owner-only data dirs
# (~/.opencodereview/sessions is 0700) without any ACL changes.
RUNAS    ?= $(shell id -u):$(shell id -g)
# The DB is bind-mounted from the host instead of a named volume: a named
# volume carries the image's staged 65534 ownership and is unwritable by
# the host uid. This is also the host CLI's own database, so `make index`
# and the container share one index.
DATA_DIR ?= $(HOME)/.local/share/pi-token-usage
PORT     ?= 7777
TZ       ?= $(shell timedatectl show -p Timezone --value 2>/dev/null || cat /etc/timezone 2>/dev/null || echo UTC)
BIN      ?= pitokens
COMPOSE  ?= docker compose
# Compose service name (docker-compose.yml). Distinct from $(NAME), which is the
# container name — overriding NAME must not break `compose exec <service>`.
SERVICE ?= pitokens

# `docker compose` interpolates ${VARS} from the process environment, not from
# make variables, so these have to be exported for the compose file to see them
# (they override the committed .env, which is the bare-compose fallback).
export IMAGE NAME SESSIONS OPENCODE_DATA OCR_HOME DATA_DIR PORT TZ RUNAS

.DEFAULT_GOAL := help

# ------------------------------------------------------------------ local

# Shared design system (base-styles repo). internal/pius/web/base.css is
# vendored and committed; refresh with `make css`. go:embed needs it before build.
BASE_STYLES_TAG ?= v1.0.0
BASE_STYLES_URL := http://gitea.rantaeskola.fi/gizmoz/base-styles/raw/tag/$(BASE_STYLES_TAG)

css: ## Fetch shared base.css from base-styles
	curl -fsSL $(BASE_STYLES_URL)/base.css -o internal/pius/web/base.css

build: css ## build the static binary
	CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o $(BIN) ./cmd/pitokens

test: ## run unit tests
	go test ./...

vet: ## go vet
	go vet ./...

index: build ## incremental index into the local db
	PI_SESSIONS_DIR=$(SESSIONS) PITOKENS_DB=$(DATA_DIR)/index.db ./$(BIN) index

index-full: build ## wipe and re-parse everything
	PI_SESSIONS_DIR=$(SESSIONS) PITOKENS_DB=$(DATA_DIR)/index.db ./$(BIN) index --full

run: build ## serve locally on the host (http://127.0.0.1:7777)
	PI_SESSIONS_DIR=$(SESSIONS) PITOKENS_ADDR=127.0.0.1:$(PORT) ./$(BIN) serve

report: build ## text report, e.g. make report BY=project
	PI_SESSIONS_DIR=$(SESSIONS) ./$(BIN) report -by $(or $(BY),month)

clean: ## remove build artifacts
	rm -f $(BIN)

# ------------------------------------------------------------------ docker

# Everything below drives docker-compose.yml, which owns the mounts, env and the
# restart policy. `restart: unless-stopped` + the enabled docker daemon = the
# dashboard is up after every boot; `make docker-down` is the only thing that
# keeps it down.

docker-build: ## build the container image
	$(COMPOSE) build

docker-up: docker-build ## start the daemon (comes back on boot via restart policy)
	# Create the host data dir first: docker would create a missing bind source
	# as root:root, which the container's host uid cannot write into.
	mkdir -p $(DATA_DIR)
	$(COMPOSE) up -d
	@echo "→ http://127.0.0.1:$(PORT)   (tz=$(TZ), uid=$(RUNAS))"

docker-down: ## stop and remove the container (data dir is a bind-mount, kept)
	$(COMPOSE) down

docker-restart: ## restart the container
	$(COMPOSE) restart

docker-logs: ## follow container logs
	$(COMPOSE) logs -f

docker-ps: ## is it running, and with what restart policy
	$(COMPOSE) ps
	@echo "restart policy: $$(docker inspect -f '{{.HostConfig.RestartPolicy.Name}}' $(NAME) 2>/dev/null || echo '(not running)')"
	@echo "docker daemon:  $$(systemctl is-enabled docker 2>/dev/null || echo '(unknown)') at boot"

docker-reindex: ## incremental re-index inside the container
	$(COMPOSE) exec $(SERVICE) /pitokens index

docker-rebuild-index: ## full re-index inside the container (clears offsets)
	$(COMPOSE) exec $(SERVICE) /pitokens index --full

docker-status: ## show index/table stats
	$(COMPOSE) exec $(SERVICE) /pitokens healthcheck && \
	$(COMPOSE) exec $(SERVICE) /pitokens report -by month | tail -12

# The database survives `docker-clean`: it is a bind-mount of $(DATA_DIR),
# not a named volume, so `down -v` cannot reach it.
docker-clean: ## remove the stack (container, network, anonymous volumes)
	$(COMPOSE) down -v --remove-orphans

docker-shell: ## run an arbitrary CLI subcommand, e.g. make docker-shell CMD="report -by project"
	$(COMPOSE) run --rm --entrypoint /pitokens $(SERVICE) $(or $(CMD),report)

help: ## show this help
	@grep -E '^[a-zA-Z_-]+:.*?## .*$$' $(MAKEFILE_LIST) \
	  | awk 'BEGIN {FS = ":.*?## "}; {printf "  \033[36m%-22s\033[0m %s\n", $$1, $$2}'

.PHONY: css build test vet index index-full run report clean \
        docker-build docker-up docker-down docker-restart docker-logs docker-ps \
        docker-reindex docker-rebuild-index docker-status docker-clean docker-shell help
