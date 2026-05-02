BINARY := observer
PKG    := github.com/marmutapp/superbased-observer
CMD    := ./cmd/observer

GO         ?= go
GOFLAGS    ?=
BUILD_DIR  := bin
COVER_OUT  := coverage.txt

.PHONY: all build test test-race lint fmt vet tidy clean run cover

all: fmt vet lint test build

build:
	@mkdir -p $(BUILD_DIR)
	$(GO) build $(GOFLAGS) -o $(BUILD_DIR)/$(BINARY) $(CMD)

run: build
	$(BUILD_DIR)/$(BINARY)

test:
	$(GO) test $(GOFLAGS) ./...

test-race:
	$(GO) test $(GOFLAGS) -race ./...

cover:
	$(GO) test $(GOFLAGS) -race -coverprofile=$(COVER_OUT) -covermode=atomic ./...
	$(GO) tool cover -func=$(COVER_OUT) | tail -1

lint:
	@command -v golangci-lint >/dev/null 2>&1 || { echo "golangci-lint not installed — see https://golangci-lint.run/"; exit 0; }
	golangci-lint run ./...

fmt:
	$(GO) fmt ./...

vet:
	$(GO) vet ./...

tidy:
	$(GO) mod tidy

clean:
	rm -rf $(BUILD_DIR) $(COVER_OUT)
