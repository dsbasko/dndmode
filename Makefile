.PHONY: build test test-cover lint generate tools clean acceptance install audit-net audit-net-runtime audit-deps release release-check

GO         ?= go
PKG        := ./...
BIN        := dndmode
GOPATH_BIN := $(shell $(GO) env GOPATH)/bin

build:
	CGO_ENABLED=1 GOOS=darwin GOARCH=arm64 $(GO) build -o $(BIN) ./cmd/dndmode
	@codesign --force --sign - --identifier com.dsbasko.dndmode ./$(BIN)
	@echo "Built $(BIN) with ad-hoc codesign (identifier=com.dsbasko.dndmode)"

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

install: build
	sudo cp ./$(BIN) /usr/local/bin/
	@echo "Verifying codesign on /usr/local/bin/$(BIN)..."
	@codesign -dvv /usr/local/bin/$(BIN) 2>&1 | grep "Identifier=com.dsbasko.dndmode" && echo "PASS: codesign identifier matches" || (echo "FAIL: codesign verification" && exit 1)

audit-deps:
	@echo "=== Static dependency audit (production binary only) ==="
	@$(GO) list -deps ./cmd/dndmode 2>/dev/null | grep -iE "(^|/)(net|http|grpc|tls|websocket|sock)(/|$$)" && echo "FAIL: network deps in production closure" && exit 1 || echo "PASS: no network deps in production closure"

audit-net: audit-deps
	@echo ""
	@echo "=== Runtime socket audit ==="
	@echo "To verify zero open sockets at runtime:"
	@echo "  1. In another terminal: ./dndmode (grant Accessibility, etc.)"
	@echo "  2. Run: make audit-net-runtime"

audit-net-runtime:
	@PID=$$(pgrep -x dndmode | head -1); \
	if [ -z "$$PID" ]; then echo "FAIL: dndmode not running (start it in another terminal first)"; exit 1; fi; \
	echo "Checking PID $$PID for open network sockets..."; \
	if lsof -p $$PID 2>/dev/null | awk 'NR==1 || $$5 ~ /(IPv4|IPv6)/ || $$8 ~ /(TCP|UDP)/' | grep -qE "(IPv4|IPv6|TCP|UDP)"; then \
		echo "FAIL: network sockets detected"; exit 1; \
	else \
		echo "PASS: no network sockets open"; \
	fi

release-check:
	@if [ -z "$(VERSION)" ]; then echo "ERROR: VERSION required (e.g., make release VERSION=1.0.0)"; exit 1; fi
	@if ! echo "$(VERSION)" | grep -qE "^[0-9]+\.[0-9]+\.[0-9]+$$"; then echo "ERROR: VERSION must be x.y.z (no leading v)"; exit 1; fi
	@if ! git diff-index --quiet HEAD --; then echo "ERROR: working tree not clean"; exit 1; fi
	@if git rev-parse --verify "v$(VERSION)" >/dev/null 2>&1; then echo "ERROR: tag v$(VERSION) already exists"; exit 1; fi

release: release-check build audit-deps
	@echo "Tagging v$(VERSION)..."
	@git tag -a "v$(VERSION)" -m "Release v$(VERSION)"
	@echo "Pushing tag..."
	@git push origin "v$(VERSION)"
	@echo "Released v$(VERSION). Users can now: go install github.com/dsbasko/dndmode@v$(VERSION)"
