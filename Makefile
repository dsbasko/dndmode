.PHONY: build test test-cover lint generate tools clean acceptance

GO         ?= go
PKG        := ./...
BIN        := dndmode
GOPATH_BIN := $(shell $(GO) env GOPATH)/bin

build:
	CGO_ENABLED=0 GOOS=darwin GOARCH=arm64 $(GO) build -o $(BIN) ./cmd/dndmode

test:
	$(GO) test -race $(PKG)

test-cover:
	$(GO) test -race -coverprofile=coverage.out -covermode=atomic $(PKG)
	$(GO) tool cover -func=coverage.out | tail -1

lint:
	$(GO) vet $(PKG)
	$(GOPATH_BIN)/golangci-lint run

generate:
	$(GO) generate $(PKG)

tools:
	$(GO) install go.uber.org/mock/mockgen@v0.6.0

acceptance:
	$(GO) test -tags=acceptance -count=1 ./cmd/dndmode

clean:
	rm -f $(BIN) coverage.out
	rm -rf internal/*/mocks internal/*/*/mocks
