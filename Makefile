# transcode — developer tasks. The CI gate is `make check`.
STATICCHECK_VERSION ?= 2024.1.1
GOVULNCHECK_VERSION ?= v1.1.3

VERSION ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo 0.0.0-dev)
COMMIT  ?= $(shell git rev-parse --short HEAD 2>/dev/null || echo unknown)
DATE    ?= $(shell date -u +%Y-%m-%dT%H:%M:%SZ)
LDFLAGS := -s -w \
  -X github.com/NSchatz/transcode/internal/version.Version=$(VERSION) \
  -X github.com/NSchatz/transcode/internal/version.Commit=$(COMMIT) \
  -X github.com/NSchatz/transcode/internal/version.Date=$(DATE)

.PHONY: build test check fmt vet staticcheck govulncheck tidy clean

build:
	CGO_ENABLED=0 go build -trimpath -ldflags="$(LDFLAGS)" -o transcode ./cmd/transcode

test:
	go test -race -covermode=atomic ./...

fmt:
	@out="$$(gofmt -l .)"; if [ -n "$$out" ]; then echo "gofmt needs:"; echo "$$out"; exit 1; fi

vet:
	go vet ./...

staticcheck:
	go run honnef.co/go/tools/cmd/staticcheck@$(STATICCHECK_VERSION) ./...

govulncheck:
	go run golang.org/x/vuln/cmd/govulncheck@$(GOVULNCHECK_VERSION) ./...

# The full CI gate, locally.
check: fmt vet build test staticcheck govulncheck

tidy:
	go mod tidy

clean:
	rm -f transcode
	rm -rf dist out
