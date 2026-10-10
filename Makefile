GO ?= go
VERSION ?= dev
BUILD_DIR ?= bin
GOOS ?= $(shell $(GO) env GOOS)
GOARCH ?= $(shell $(GO) env GOARCH)
CGO_ENABLED ?= 0
OUTPUT ?= $(BUILD_DIR)/db-agent$(if $(filter windows,$(GOOS)),.exe,)
LDFLAGS ?= -X github.com/fivemanage/db-agent/cmd.version=$(VERSION)

.DEFAULT_GOAL := build
.PHONY: build build-all test vet check

build:
	mkdir -p "$(dir $(OUTPUT))"
	CGO_ENABLED=$(CGO_ENABLED) GOOS=$(GOOS) GOARCH=$(GOARCH) $(GO) build -trimpath -ldflags "$(LDFLAGS)" -o "$(OUTPUT)" .

build-all:
	$(MAKE) build GOOS=windows GOARCH=amd64 OUTPUT="$(BUILD_DIR)/db-agent-windows-amd64.exe"
	$(MAKE) build GOOS=linux GOARCH=amd64 OUTPUT="$(BUILD_DIR)/db-agent-linux-amd64"
	$(MAKE) build GOOS=linux GOARCH=arm64 OUTPUT="$(BUILD_DIR)/db-agent-linux-arm64"
	$(MAKE) build GOOS=darwin GOARCH=arm64 OUTPUT="$(BUILD_DIR)/db-agent-darwin-arm64"

test:
	$(GO) test ./...

vet:
	$(GO) vet ./...

check: test vet
