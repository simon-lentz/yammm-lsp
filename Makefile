# yammm-lsp Language Server

LSP_BINARY = yammm-lsp
LSP_CMD = ./cmd/yammm-lsp
VERSION ?= dev
LSP_LDFLAGS = -ldflags="-s -w -X main.version=$(VERSION)"

# Binary output directory (editor-agnostic)
LSP_BIN = bin

# VS Code extension directory
VSCODE_EXT = editors/vscode

# Detect current platform
GOOS ?= $(shell go env GOOS)
GOARCH ?= $(shell go env GOARCH)
ifeq ($(GOOS),windows)
  BINARY_EXT = .exe
else
  BINARY_EXT =
endif

.PHONY: lint
lint:
	go tool golangci-lint run

.PHONY: lint-fix
lint-fix:
	go tool golangci-lint run --fix

.PHONY: test
test:
	go test ./...

# Build LSP server for current platform (output to working directory)
.PHONY: build
build:
	go build $(LSP_LDFLAGS) -o $(LSP_BINARY) $(LSP_CMD)

# Build LSP server for current (native) platform into bin/
.PHONY: build-native
build-native:
	@mkdir -p $(LSP_BIN)/$(GOOS)-$(GOARCH)
	GOOS=$(GOOS) GOARCH=$(GOARCH) go build $(LSP_LDFLAGS) -o $(LSP_BIN)/$(GOOS)-$(GOARCH)/$(LSP_BINARY)$(BINARY_EXT) $(LSP_CMD)

# Cross-compile LSP server for all platforms
.PHONY: build-all
build-all: build-darwin-arm64 build-darwin-amd64 build-linux-amd64 build-linux-arm64 build-windows-amd64 build-windows-arm64

.PHONY: build-darwin-arm64
build-darwin-arm64:
	@mkdir -p $(LSP_BIN)/darwin-arm64
	GOOS=darwin GOARCH=arm64 go build $(LSP_LDFLAGS) -o $(LSP_BIN)/darwin-arm64/$(LSP_BINARY) $(LSP_CMD)

.PHONY: build-darwin-amd64
build-darwin-amd64:
	@mkdir -p $(LSP_BIN)/darwin-amd64
	GOOS=darwin GOARCH=amd64 go build $(LSP_LDFLAGS) -o $(LSP_BIN)/darwin-amd64/$(LSP_BINARY) $(LSP_CMD)

.PHONY: build-linux-amd64
build-linux-amd64:
	@mkdir -p $(LSP_BIN)/linux-amd64
	GOOS=linux GOARCH=amd64 go build $(LSP_LDFLAGS) -o $(LSP_BIN)/linux-amd64/$(LSP_BINARY) $(LSP_CMD)

.PHONY: build-linux-arm64
build-linux-arm64:
	@mkdir -p $(LSP_BIN)/linux-arm64
	GOOS=linux GOARCH=arm64 go build $(LSP_LDFLAGS) -o $(LSP_BIN)/linux-arm64/$(LSP_BINARY) $(LSP_CMD)

.PHONY: build-windows-amd64
build-windows-amd64:
	@mkdir -p $(LSP_BIN)/windows-amd64
	GOOS=windows GOARCH=amd64 go build $(LSP_LDFLAGS) -o $(LSP_BIN)/windows-amd64/$(LSP_BINARY).exe $(LSP_CMD)

.PHONY: build-windows-arm64
build-windows-arm64:
	@mkdir -p $(LSP_BIN)/windows-arm64
	GOOS=windows GOARCH=arm64 go build $(LSP_LDFLAGS) -o $(LSP_BIN)/windows-arm64/$(LSP_BINARY).exe $(LSP_CMD)

# Copy LSP binaries into VS Code extension for packaging
.PHONY: copy-to-vscode
copy-to-vscode:
	rm -rf $(VSCODE_EXT)/bin
	cp -r $(LSP_BIN) $(VSCODE_EXT)/bin

# Build VS Code extension (native platform only, for development)
.PHONY: build-vscode
build-vscode: build-native copy-to-vscode
	cd $(VSCODE_EXT) && npm ci --no-audit && npm run compile

# Build VS Code extension for all platforms (for releases)
.PHONY: build-vscode-all
build-vscode-all: build-all copy-to-vscode
	cd $(VSCODE_EXT) && npm ci --no-audit && npm run compile

# Package VS Code extension
.PHONY: package-vscode
package-vscode: build-vscode
	cd $(VSCODE_EXT) && npm run package

# Clean build artifacts
.PHONY: clean
clean:
	rm -f $(LSP_BINARY)
	rm -rf $(LSP_BIN)
	rm -rf $(VSCODE_EXT)/bin
