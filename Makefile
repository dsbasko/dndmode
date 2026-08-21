.PHONY: build test test-cover lint generate tools clean acceptance install audit-net audit-net-runtime audit-deps release release-check version-check package release-publish

GO         ?= go
PKG        := ./...
BIN        := dndmode
GOPATH_BIN := $(shell $(GO) env GOPATH)/bin
REPO       ?= dsbasko/dndmode
DIST       := dist
ARCHIVE    := $(DIST)/$(BIN)_v$(VERSION)_darwin_arm64.tar.gz
CHECKSUM   := $(ARCHIVE).sha256

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
	rm -rf $(DIST)
	rm -rf internal/*/mocks internal/*/*/mocks

install: build
	# Remove the old binary BEFORE copying so the destination gets a FRESH inode.
	# An in-place `cp` overwrite reuses the existing /usr/local/bin/$(BIN) inode,
	# whose ad-hoc code signature the kernel cached on a prior exec. The new bytes
	# then fail cs validation ("Taskgated Invalid Signature") and the process is
	# SIGKILLed at launch on Apple Silicon (CODESIGNING corpse, "Code Signature
	# Invalid"), even though `codesign --verify` passes on disk. rm + cp forces a
	# new inode with no stale cache.
	sudo rm -f /usr/local/bin/$(BIN)
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

version-check:
	@if [ -z "$(VERSION)" ]; then echo "ERROR: VERSION required (e.g., make release VERSION=1.0.0)"; exit 1; fi
	@if ! echo "$(VERSION)" | grep -qE "^[0-9]+\.[0-9]+\.[0-9]+$$"; then echo "ERROR: VERSION must be x.y.z (no leading v)"; exit 1; fi

release-check: version-check
	@BRANCH=$$(git rev-parse --abbrev-ref HEAD); \
	if [ "$$BRANCH" != "main" ] && [ "$$BRANCH" != "master" ]; then \
		echo "ERROR: release must be tagged from main/master branch (current: $$BRANCH)"; exit 1; \
	fi
	@if ! git diff-index --quiet HEAD --; then echo "ERROR: working tree not clean"; exit 1; fi
	@if git rev-parse --verify "v$(VERSION)" >/dev/null 2>&1; then echo "ERROR: tag v$(VERSION) already exists"; exit 1; fi

# Packages the codesigned darwin/arm64 binary into a downloadable release asset.
# The ad-hoc signature lives inside the Mach-O (LC_CODE_SIGNATURE), not in an
# xattr, so a plain tar round-trip preserves it — but the archive is verified
# below anyway: an unsigned binary in a Release would SIGKILL on Apple Silicon.
package: version-check build
	@rm -rf $(DIST)
	@mkdir -p $(DIST)
	@tar -czf $(ARCHIVE) $(BIN)
	@rm -rf $(DIST)/verify && mkdir -p $(DIST)/verify
	@tar -xzf $(ARCHIVE) -C $(DIST)/verify
	@codesign -dvv $(DIST)/verify/$(BIN) 2>&1 | grep -q "Identifier=com.dsbasko.dndmode" \
		|| (echo "FAIL: packaged binary lost its codesign identifier" && exit 1)
	@rm -rf $(DIST)/verify
	@cd $(DIST) && shasum -a 256 "$(BIN)_v$(VERSION)_darwin_arm64.tar.gz" > "$(BIN)_v$(VERSION)_darwin_arm64.tar.gz.sha256"
	@echo "PASS: packaged $(ARCHIVE) (codesign identifier intact)"
	@cat $(CHECKSUM)

# Creates the GitHub Release page (or tops up an existing one) and attaches the
# archive + checksum. Optional: HEADLINE="short title", NOTES=path/to/notes.md.
release-publish: version-check
	@# One shell for the whole recipe: `exit 0` in the no-gh branch must skip the
	@# rest, and make would otherwise start a fresh shell per line and run it anyway.
	@if [ ! -f "$(ARCHIVE)" ]; then echo "ERROR: $(ARCHIVE) not found — run 'make package VERSION=$(VERSION)' first"; exit 1; fi; \
	if ! command -v gh >/dev/null 2>&1; then \
		echo "WARN: gh CLI not found — Release page untouched. Attach the assets manually:"; \
		echo "  gh release create v$(VERSION) --repo $(REPO) --title \"v$(VERSION) — <headline>\" --notes-file <notes> \\"; \
		echo "    $(ARCHIVE) $(CHECKSUM)"; \
		exit 0; \
	fi; \
	TITLE="v$(VERSION)"; \
	if [ -n "$(HEADLINE)" ]; then TITLE="v$(VERSION) — $(HEADLINE)"; fi; \
	if gh release view "v$(VERSION)" --repo "$(REPO)" >/dev/null 2>&1; then \
		echo "Release v$(VERSION) already exists — uploading assets..."; \
		gh release upload "v$(VERSION)" "$(ARCHIVE)" "$(CHECKSUM)" --repo "$(REPO)" --clobber; \
	elif [ -n "$(NOTES)" ]; then \
		echo "Creating Release $$TITLE with assets..."; \
		gh release create "v$(VERSION)" "$(ARCHIVE)" "$(CHECKSUM)" --repo "$(REPO)" --title "$$TITLE" --notes-file "$(NOTES)"; \
	else \
		echo "Creating Release $$TITLE with assets (auto-generated notes)..."; \
		gh release create "v$(VERSION)" "$(ARCHIVE)" "$(CHECKSUM)" --repo "$(REPO)" --title "$$TITLE" --generate-notes; \
	fi

release: release-check build audit-deps package
	@echo "Tagging v$(VERSION)..."
	@git tag -a "v$(VERSION)" -m "Release v$(VERSION)"
	@echo "Pushing tag..."
	@git push origin "v$(VERSION)"
	@$(MAKE) --no-print-directory release-publish VERSION=$(VERSION) HEADLINE="$(HEADLINE)" NOTES="$(NOTES)"
	@echo "Released v$(VERSION). Users can now: go install github.com/dsbasko/dndmode@v$(VERSION)"
	@echo "Downloadable asset: $(notdir $(ARCHIVE))"
