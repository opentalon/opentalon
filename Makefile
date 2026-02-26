.PHONY: all build test lint deps setup plugin channel run clean

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
	@cd $(HELLO_WORLD_DIR) && go build -o hello-world-plugin .

channel: $(CONSOLE_BIN)

$(CONSOLE_BIN):
	@if [ ! -d "$(CONSOLE_DIR)" ]; then \
		echo "Cloning console channel..."; \
		git clone --depth 1 $(CONSOLE_REPO) $(CONSOLE_DIR); \
	fi
	@echo "Building console channel..."
	@cd $(CONSOLE_DIR) && go build -o console ./cmd/console

# ── Core ────────────────────────────────────────────────────────────────────

deps:
	@command -v golangci-lint >/dev/null 2>&1 || { echo "Installing golangci-lint..."; go install github.com/golangci/golangci-lint/v2/cmd/golangci-lint@v2.7.2; }

build:
	go build -o $(BINARY) $(CMD_PKG)

test:
	go test -race -v ./...

lint:
	golangci-lint run

# ── Convenience ─────────────────────────────────────────────────────────────

run: build setup
	@echo ""
	@echo "Starting OpenTalon..."
	@./$(BINARY) -config config.yaml

clean:
	rm -f $(BINARY)
	rm -f $(HELLO_WORLD_BIN)
	rm -f $(CONSOLE_BIN)
