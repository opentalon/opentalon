.PHONY: all build test lint deps setup plugin channel run clean clean-cache clean-plugins clean-channels clean-skills clean-lua-plugins proto vcr-record-all

BINARY      := opentalon
CMD_PKG     := ./cmd/opentalon
PLUGIN_DIR  := ./plugins
CHANNEL_DIR := ./channels

# Plugin / channel repos
HELLO_WORLD_REPO := https://github.com/opentalon/hellow-world-plugin.git
HELLO_WORLD_DIR  := $(PLUGIN_DIR)/hellow-world-plugin
HELLO_WORLD_BIN  := $(HELLO_WORLD_DIR)/hello-world-plugin

CONSOLE_REPO     := https://github.com/opentalon/console-channel.git
CONSOLE_DIR      := $(CHANNEL_DIR)/console-channel
CONSOLE_BIN      := $(CONSOLE_DIR)/console

all: deps build test lint

# ── Setup: clone and build external plugins & channels ──────────────────────

setup: plugin channel

plugin: $(HELLO_WORLD_BIN)

$(HELLO_WORLD_BIN):
	@if [ ! -d "$(HELLO_WORLD_DIR)" ]; then \
		echo "Cloning hello-world plugin..."; \
		git clone --depth 1 $(HELLO_WORLD_REPO) $(HELLO_WORLD_DIR); \
	fi
	@echo "Building hello-world plugin..."
	@cd $(HELLO_WORLD_DIR) && go mod edit -replace github.com/opentalon/opentalon=../.. && go mod tidy && go build -o hello-world-plugin .

channel: $(CONSOLE_BIN)

$(CONSOLE_BIN):
	@if [ ! -d "$(CONSOLE_DIR)" ]; then \
		echo "Cloning console channel..."; \
		git clone --depth 1 $(CONSOLE_REPO) $(CONSOLE_DIR); \
	fi
	@echo "Building console channel..."
	@cd $(CONSOLE_DIR) && go mod edit -replace github.com/opentalon/opentalon=../.. && go mod tidy && go build -o console ./cmd/console

# ── Proto ───────────────────────────────────────────────────────────────────

proto:
	buf generate

# ── Core ────────────────────────────────────────────────────────────────────

deps:
	@command -v golangci-lint >/dev/null 2>&1 || { echo "Installing golangci-lint..."; go install github.com/golangci/golangci-lint/v2/cmd/golangci-lint@v2.7.2; }

build:
	go build -o $(BINARY) $(CMD_PKG)

test:
	go test -race -v ./...

lint:
	golangci-lint run

# ── VCR cassettes ────────────────────────────────────────────────────────────
# Re-record all VCR cassettes against the real Anthropic API.
# Requires: ANTHROPIC_API_KEY to be set.
# Run this after changing anything in internal/prompts/.
vcr-record-all:
	@if [ -z "$$ANTHROPIC_API_KEY" ]; then \
		echo "error: ANTHROPIC_API_KEY is not set"; exit 1; \
	fi
	VCR_RECORD=1 go test -v -run TestVCR ./internal/orchestrator/...

# ── Convenience ─────────────────────────────────────────────────────────────

run: build setup
	@echo ""
	@echo "Starting OpenTalon..."
	@./$(BINARY) -config config.yaml

clean:
	rm -f $(BINARY)
	rm -f $(HELLO_WORLD_BIN)
	rm -f $(CONSOLE_BIN)

# ── Clean cache: clear cached plugins/channels/skills ──────────────────────

clean-cache: build
	@./$(BINARY) -config config.yaml -clean all

clean-plugins: build
	@./$(BINARY) -config config.yaml -clean plugins

clean-channels: build
	@./$(BINARY) -config config.yaml -clean channels

clean-skills: build
	@./$(BINARY) -config config.yaml -clean skills

clean-lua-plugins: build
	@./$(BINARY) -config config.yaml -clean lua_plugins
