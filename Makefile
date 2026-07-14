# transcode — developer tasks.
#
# `make check` IS the gate, and this file is where it is defined: CI runs it, the release
# workflow runs it, and so do you. The tool pins below are therefore the only ones — they
# used to be restated in ci.yml, which meant bumping one drifted the PR gate away from
# the release gate. (CI adds two things on top: a config-schema self-test, and the image
# smoke gate — scripts/smoke-image.sh — which needs Docker.)
STATICCHECK_VERSION ?= 2025.1.1
GOVULNCHECK_VERSION ?= v1.1.4

VERSION ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo 0.0.0-dev)
COMMIT  ?= $(shell git rev-parse --short HEAD 2>/dev/null || echo unknown)
DATE    ?= $(shell date -u +%Y-%m-%dT%H:%M:%SZ)
LDFLAGS := -s -w \
  -X github.com/NSchatz/transcode/internal/version.Version=$(VERSION) \
  -X github.com/NSchatz/transcode/internal/version.Commit=$(COMMIT) \
  -X github.com/NSchatz/transcode/internal/version.Date=$(DATE)

IMAGE    ?= transcode:dev
PLATFORM ?= linux/amd64

.PHONY: build test check fmt vet staticcheck govulncheck check-pins tidy clean image image-smoke compose-check

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

# Cross-file pins must AGREE, not merely be asked to. NOTICE has to name the exact ffmpeg
# the image bundles (it is the GPL source offer, and it ships inside the image), and the
# gate has to run on the Go that builds the shipped binary. Nothing else forces either.
check-pins:
	./scripts/check-pins.sh

# THE gate. CI and the release workflow both run exactly this.
check: check-pins fmt vet build test staticcheck govulncheck

# --- packaging (TRANSCODE-9) --------------------------------------------------
# The same commands CI runs, so the packaging gate is reproducible by a human and not
# something only the runner can do.
image:
	docker buildx build --platform $(PLATFORM) --load -t $(IMAGE) \
	  --build-arg VERSION=$(VERSION) --build-arg COMMIT=$(COMMIT) --build-arg DATE=$(DATE) .

# Builds the image, then drives a REAL oneshot encode inside it and asserts the
# no-loss contract held. This — not "the build succeeded" — is the packaging gate.
image-smoke: image
	./scripts/smoke-image.sh $(IMAGE)

compose-check:
	docker compose -f docker-compose.yml config -q && echo "docker-compose.yml is valid"

tidy:
	go mod tidy

clean:
	rm -f transcode
	rm -rf dist out
