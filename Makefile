SHELL := /bin/sh

PROJECT_DIR := $(abspath $(dir $(lastword $(MAKEFILE_LIST))))
BIN_DIR ?= $(PROJECT_DIR)/bin

# Prefer the local Go 1.27 toolchain used for this project when it exists.
LOCAL_GO := /Users/Cage/bin/bin/go
GO ?= $(if $(wildcard $(LOCAL_GO)),$(LOCAL_GO),go)
WINDOWS_CC ?= x86_64-w64-mingw32-gcc
GO_BUILD_FLAGS ?= -trimpath

.PHONY: build build-arm build-win build-darwin-arm64 build-windows-amd64 check-go check-windows-cc clean

build: build-arm build-win

build-arm: check-go
	@mkdir -p "$(BIN_DIR)"
	CGO_ENABLED=1 GOOS=darwin GOARCH=arm64 $(GO) build $(GO_BUILD_FLAGS) -o "$(BIN_DIR)/jdtls-go-darwin-arm64" ./cmd/jdtls-go
	CGO_ENABLED=1 GOOS=darwin GOARCH=arm64 $(GO) build $(GO_BUILD_FLAGS) -o "$(BIN_DIR)/java-debug-go-darwin-arm64" ./cmd/java-debug-go

build-win: check-go check-windows-cc
	@mkdir -p "$(BIN_DIR)"
	CGO_ENABLED=1 GOOS=windows GOARCH=amd64 CC="$(WINDOWS_CC)" $(GO) build $(GO_BUILD_FLAGS) -o "$(BIN_DIR)/jdtls-go-windows-amd64.exe" ./cmd/jdtls-go
	CGO_ENABLED=1 GOOS=windows GOARCH=amd64 CC="$(WINDOWS_CC)" $(GO) build $(GO_BUILD_FLAGS) -o "$(BIN_DIR)/java-debug-go-windows-amd64.exe" ./cmd/java-debug-go

# Keep the explicit platform names as aliases for existing scripts.
build-darwin-arm64: build-arm
build-windows-amd64: build-win

check-go:
	@version=`$(GO) version 2>/dev/null` || { echo "Go compiler not found: $(GO)" >&2; exit 1; }; \
	case "$$version" in *go1.27.*) ;; *) echo "Go 1.27 is required; found: $$version" >&2; exit 1 ;; esac

check-windows-cc:
	@command -v "$(WINDOWS_CC)" >/dev/null 2>&1 || { \
		echo "Windows amd64 C compiler not found: $(WINDOWS_CC)" >&2; \
		echo "Install MinGW-w64 and/or run make build-win WINDOWS_CC=/path/to/x86_64-w64-mingw32-gcc" >&2; \
		exit 1; \
	}

clean:
	rm -f "$(BIN_DIR)/jdtls-go-darwin-arm64" \
	      "$(BIN_DIR)/java-debug-go-darwin-arm64" \
	      "$(BIN_DIR)/jdtls-go-windows-amd64.exe" \
	      "$(BIN_DIR)/java-debug-go-windows-amd64.exe"
