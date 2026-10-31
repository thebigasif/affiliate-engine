BIN      := affiliate-engine
GO       := $(shell command -v go 2>/dev/null || echo $(HOME)/gotoolchain/bin/go)

.PHONY: build test test-race vet fmt gofumpt gosec govulncheck bench clean crossbuild release-dist test-cover

build:
	$(GO) build -o $(BIN) ./cmd/affiliate-engine

test:
	$(GO) test ./... -count=1

test-race:
	$(GO) test ./... -race -count=1

vet:
	$(GO) vet ./...

fmt:
	$(GO) fmt ./...

# Formatting gate (stricter than gofmt): fails if any file would change.
gofumpt:
	@out=$$($(GO) run mvdan.cc/gofumpt@latest -l . 2>/dev/null | grep -v '^research/' || true); \
	if [ -n "$$out" ]; then echo "gofumpt would reformat:"; echo "$$out"; exit 1; fi

# Security gate: zero findings (14 reviewed #nosec annotations carry justifications).
gosec:
	$(GO) run github.com/securego/gosec/v2/cmd/gosec@latest -quiet ./...

govulncheck:
	$(GO) run golang.org/x/vuln/cmd/govulncheck@latest ./...

# All lint gates in one target.
lint: vet gofumpt gosec govulncheck
	$(GO) build -C tools/staticcheck -o $(CURDIR)/.bin/staticcheck honnef.co/go/tools/cmd/staticcheck
	.bin/staticcheck ./...

bench:
	$(GO) test ./internal/clicks ./internal/analytics -bench=. -benchmem -run=^$$

crossbuild:
	CGO_ENABLED=0 GOOS=darwin GOARCH=arm64 $(GO) build -ldflags="-s -w" -o dist/affiliate-engine-darwin-arm64 ./cmd/affiliate-engine
	CGO_ENABLED=0 GOOS=linux GOARCH=amd64 $(GO) build -ldflags="-s -w" -o dist/affiliate-engine-linux-amd64 ./cmd/affiliate-engine
	CGO_ENABLED=0 GOOS=linux GOARCH=arm64 $(GO) build -ldflags="-s -w" -o dist/affiliate-engine-linux-arm64 ./cmd/affiliate-engine
	@n=$$(shasum -a 256 dist/affiliate-engine-darwin-arm64 dist/affiliate-engine-linux-amd64 dist/affiliate-engine-linux-arm64 | cut -d' ' -f1 | sort -u | wc -l | tr -d ' '); \
	if [ "$$n" -ne 3 ]; then echo "crossbuild: only $$n distinct binaries for 3 targets — GOOS/GOARCH not applied"; exit 1; fi

clean:
	rm -f $(BIN) dist/*

# NOTE (bug class, fixed session 17): this loop historically computed
# $$os/$$arch but never exported GOOS/GOARCH to the build — every target
# silently compiled for the HOST. Five "cross-compiled" artifacts were
# five copies of one host binary (caught because their sha256s were
# identical; CI never noticed because the ubuntu smoke test only ran the
# linux/amd64 name, which was the host arch). The per-invocation guard
# below makes that class impossible to ship again.
release-dist:
	@mkdir -p dist
	@rm -f dist/affiliate-engine-* dist/checksums.txt
	@for target in linux/amd64 linux/arm64 darwin/amd64 darwin/arm64 windows/amd64; do \
		os=$${target%/*}; arch=$${target#*/}; ext=""; \
		if [ "$$os" = "windows" ]; then ext=".exe"; fi; \
		echo "building $$os/$$arch"; \
		CGO_ENABLED=0 GOOS=$$os GOARCH=$$arch $(GO) build -trimpath -ldflags="-s -w -X main.version=$${VERSION:-dev}" \
			-o dist/affiliate-engine-$$os-$$arch$$ext ./cmd/affiliate-engine || exit 1; \
	done
	@cd dist && shasum -a 256 affiliate-engine-* > checksums.txt && cat checksums.txt
	@n=$$(cd dist && cut -d' ' -f1 checksums.txt | sort -u | wc -l | tr -d ' '); \
	if [ "$$n" -ne 5 ]; then echo "release-dist: only $$n distinct binaries for 5 targets — GOOS/GOARCH not applied"; exit 1; fi

test-cover:
	$(GO) test ./... -count=1 -covermode=atomic -coverprofile=cover.out
	$(GO) tool cover -func=cover.out | tail -1
