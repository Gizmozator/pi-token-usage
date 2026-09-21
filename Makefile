IMAGE    ?= pi-token-usage:latest
NAME     ?= pitokens
SESSIONS ?= $(HOME)/.pi/agent/sessions
OPENCODE ?= $(HOME)/.local/share/opencode
OCR_HOME ?= $(HOME)/.opencodereview
# Only mount the opencode dir when its db exists, so docker doesn't create an
# empty root-owned directory on machines without opencode.
OPENCODE_MOUNT := $(shell [ -f "$(OPENCODE)/opencode.db" ] && echo "-v $(OPENCODE):/opencode:ro")
OPENCODE_ENV   := $(shell [ -f "$(OPENCODE)/opencode.db" ] && echo "-e OPENCODE_DB=/opencode/opencode.db")
# ocr: mount the whole ~/.opencodereview (sessions + config.json for the
# provider name) read-only, only when the sessions dir exists.
OCR_MOUNT    := $(shell [ -d "$(OCR_HOME)/sessions" ] && echo "-v $(OCR_HOME):/ocr:ro")
OCR_ENV      := $(shell [ -d "$(OCR_HOME)/sessions" ] && echo "-e OCR_SESSIONS_DIR=/ocr/sessions")
DB_VOL   ?= pitokens-data
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

.DEFAULT_GOAL := help

# ------------------------------------------------------------------ local

build: ## build the static binary
	CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o $(BIN) ./cmd/pitokens

test: ## run unit tests
	go test ./...

vet: ## go vet
	go vet ./...

index: build ## incremental index into the local db
	PI_SESSIONS_DIR=$(SESSIONS) ./$(BIN) index

index-full: build ## wipe and re-parse everything
	PI_SESSIONS_DIR=$(SESSIONS) ./$(BIN) index --full

run: build ## serve locally (http://127.0.0.1:$(PORT))
	PI_SESSIONS_DIR=$(SESSIONS) PITOKENS_ADDR=127.0.0.1:$(PORT) ./$(BIN) serve

report: build ## text report, e.g. make report BY=project
	PI_SESSIONS_DIR=$(SESSIONS) ./$(BIN) report -by $(or $(BY),month)

clean: ## remove build artifacts
	rm -f $(BIN)

# ------------------------------------------------------------------ docker

docker-build: ## build the container image
	docker build -t $(IMAGE) .

docker-up: docker-build ## start with correct mounts (sessions read-only, db bind-mounted)
	-docker rm -f $(NAME) >/dev/null 2>&1
	mkdir -p $(DATA_DIR)
	docker run -d --name $(NAME) \
	  --user $(RUNAS) \
	  -p 127.0.0.1:$(PORT):7777 \
	  -v "$(SESSIONS):/sessions:ro" \
	  $(OPENCODE_MOUNT) \
	  $(OCR_MOUNT) \
	  -v "$(DATA_DIR):/data" \
	  -e TZ=$(TZ) \
	  -e PI_SESSIONS_DIR=/sessions \
	  $(OPENCODE_ENV) \
	  $(OCR_ENV) \
	  -e PITOKENS_DB=/data/index.db \
	  -e PITOKENS_ADDR=0.0.0.0:7777 \
	  --read-only --tmpfs /tmp \
	  $(IMAGE)
	@echo "→ http://127.0.0.1:$(PORT)   (tz=$(TZ), uid=$(RUNAS))"

docker-down: ## stop the container
	docker stop $(NAME)

docker-restart: ## restart the container
	docker restart $(NAME)

docker-logs: ## follow container logs
	docker logs -f $(NAME)

docker-reindex: ## incremental re-index inside the container
	docker exec $(NAME) /pitokens index

docker-rebuild-index: ## full re-index inside the container (clears offsets)
	docker exec $(NAME) /pitokens index --full

docker-status: ## show index/table stats
	docker exec $(NAME) /pitokens healthcheck && \
	docker exec $(NAME) /pitokens report -by month | tail -12

docker-clean: ## remove container AND the database volume
	-docker rm -f $(NAME) >/dev/null 2>&1
	-docker volume rm $(DB_VOL) >/dev/null 2>&1

docker-shell: ## run an arbitrary CLI subcommand, e.g. make docker-shell CMD="report -by project"
	docker run --rm -it \
	  --user $(RUNAS) \
	  -v "$(SESSIONS):/sessions:ro" \
	  $(OPENCODE_MOUNT) \
	  $(OCR_MOUNT) \
	  -v "$(DATA_DIR):/data" \
	  -e TZ=$(TZ) -e PI_SESSIONS_DIR=/sessions $(OPENCODE_ENV) $(OCR_ENV) -e PITOKENS_DB=/data/index.db \
	  --entrypoint /pitokens $(IMAGE) $(or $(CMD),report)

help: ## show this help
	@grep -E '^[a-zA-Z_-]+:.*?## .*$$' $(MAKEFILE_LIST) \
	  | awk 'BEGIN {FS = ":.*?## "}; {printf "  \033[36m%-22s\033[0m %s\n", $$1, $$2}'

.PHONY: build test vet index index-full run report clean \
        docker-build docker-up docker-down docker-restart docker-logs \
        docker-reindex docker-rebuild-index docker-status docker-clean docker-shell help
