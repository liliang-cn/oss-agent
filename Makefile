# oss-agent — build / dev / deploy
# The committed web/dist is embedded into the binary, so `build` does NOT require
# Node. Run `make web` only after changing the front-end.

BIN        := oss-agent
HOST       ?= linode-jp
REMOTE_DIR ?= /opt/oss-agent
DIST       := dist/oss-agent-linux-amd64

.PHONY: build web build-linux run fmt vet test check deploy clean help

help: ## list targets
	@grep -E '^[a-zA-Z_-]+:.*?## ' $(MAKEFILE_LIST) | awk 'BEGIN{FS=":.*?## "}{printf "  \033[36m%-14s\033[0m %s\n",$$1,$$2}'

build: ## build local binary (uses committed web/dist)
	go build -o $(BIN) ./cmd/oss-agent

web: ## rebuild the embedded web UI (after front-end changes)
	cd web && npm install && npm run build

build-linux: ## cross-compile a static linux/amd64 binary (embeds web/dist)
	CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -o $(DIST) ./cmd/oss-agent

run: build ## build + serve locally (expects OSS_* env set)
	./$(BIN) serve

fmt: ## gofmt the source
	gofmt -w cmd internal web/embed.go

vet: ## go vet
	go vet ./...

test: ## go test
	go test ./...

check: fmt vet test ## fmt + vet + test

deploy: build-linux ## cross-compile + push binary + restart service on HOST (must be provisioned; see docs/ONBOARDING.md)
	ssh $(HOST) 'mkdir -p $(REMOTE_DIR)'
	scp -q $(DIST) $(HOST):$(REMOTE_DIR)/oss-agent.new
	ssh $(HOST) 'chmod +x $(REMOTE_DIR)/oss-agent.new && mv $(REMOTE_DIR)/oss-agent.new $(REMOTE_DIR)/oss-agent && systemctl restart $(BIN) && sleep 3 && systemctl is-active $(BIN)'

push-db: ## copy the local knowledge DB to HOST (rebuild-free deploy of the index)
	scp -q data/knowledge.db $(HOST):$(REMOTE_DIR)/data/knowledge.db
	ssh $(HOST) 'systemctl restart $(BIN)'

clean: ## remove build artifacts
	rm -f $(BIN) $(DIST)
