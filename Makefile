# holdfast — developer tasks.
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

# The Corresponding Source URL the served page offers (AGPL-3.0 section 13, LICENSE-3).
# It rides the SAME -ldflags invocation as the version stamp below, because the offer is
# the link PLUS the build identity and the two have to name the same tree.
#
# A fork running a MODIFIED holdfast over a network points it at its own tree, and does
# not patch the embedded HTML to do it:
#
#   make build SOURCE_URL=https://git.example.org/me/holdfast
#   make image SOURCE_URL=https://git.example.org/me/holdfast
#
# The value must be an absolute http:// or https:// URL; `serve` REFUSES to start on
# anything else rather than serving an offer nobody can follow. This default is checked
# against internal/sourceoffer.Upstream by a test inside `make check`, so this copy
# cannot drift from the built-in one.
#
# Its -X assignment below is SINGLE-QUOTED, which the others do not need to be: go splits
# the -ldflags value with shell-like quoting, so an unquoted URL carrying a space would be
# split into two flags and fail the build. A value is only ever escaped at render time,
# never rejected for being unclean, so the build path carries the same values the page does.
SOURCE_URL ?= https://github.com/NSchatz/holdfast

LDFLAGS := -s -w \
  -X github.com/NSchatz/holdfast/internal/version.Version=$(VERSION) \
  -X github.com/NSchatz/holdfast/internal/version.Commit=$(COMMIT) \
  -X github.com/NSchatz/holdfast/internal/version.Date=$(DATE) \
  -X 'github.com/NSchatz/holdfast/internal/sourceoffer.URL=$(SOURCE_URL)'

IMAGE    ?= holdfast:dev
PLATFORM ?= linux/amd64

.PHONY: build test check fmt vet staticcheck govulncheck govulncheck-selftest \
        check-pins check-pins-selftest install-ffmpeg-selftest check-pin-live \
        find-browser find-browser-selftest \
        release-shape release-shape-selftest \
        webui-gen webui-stale webui-check webui-repeat-check webui-graders-selftest \
        tidy clean image image-smoke compose-check

build:
	CGO_ENABLED=0 go build -trimpath -ldflags="$(LDFLAGS)" -o holdfast ./cmd/holdfast

test:
	go test -race -covermode=atomic ./...

fmt:
	@out="$$(gofmt -l .)"; if [ -n "$$out" ]; then echo "gofmt needs:"; echo "$$out"; exit 1; fi

vet:
	go vet ./...

staticcheck:
	go run honnef.co/go/tools/cmd/staticcheck@$(STATICCHECK_VERSION) ./...

# The vulnerability gate, and deliberately stricter than govulncheck's own exit status:
# that one is non-zero only for advisories your code is proven to CALL, so an advisory in
# a module you merely import is printed and then ignored. Here every advisory the report
# names must be answered: take the fix, or record it in .govulncheck-suppressions.yaml,
# which is the only surface that can make one non-fatal.
govulncheck:
	./scripts/govulncheck.sh $(GOVULNCHECK_VERSION)

# Proves the vulnerability gate still BITES. Every way that gate can fail is a way it
# fails SILENTLY: a skipped malformed record, a stale entry, an empty report from a
# scanner that crashed. So each is defeated on purpose here, on every run.
govulncheck-selftest:
	./scripts/govulncheck-selftest.sh

# Cross-file pins must AGREE, not merely be asked to. NOTICE has to name the exact ffmpeg
# the image bundles (it is the GPL source offer, and it ships inside the image), and the
# gate has to run on the Go that builds the shipped binary. Nothing else forces either.
check-pins:
	./scripts/check-pins.sh

# Proves check-pins.sh still BITES. While being written, the rename guard degraded to a
# silent GREEN seven times — each while printing "ok" — for seven different reasons (see the
# script's header). A guard nobody tries to defeat is a guard nobody knows works.
check-pins-selftest:
	./scripts/check-pins-selftest.sh

# Proves scripts/install-ffmpeg.sh FAILS THE WAY IT SAYS IT DOES. The pinned ffmpeg is
# both the encoder the image ships and the libvmaf instrument the no-loss verdict is
# measured with, so the one thing its installer must never do is quietly end up with
# some other ffmpeg, or a half-unpacked one, on disk. Every failure mode (expired pin,
# unreachable upstream, digest mismatch, unusable archive, unwritable destination) is
# driven for real here, offline, and each is asserted to be distinct, self-describing
# and to leave NO ffmpeg behind. A guard nobody tries to defeat is a guard nobody knows
# works.
install-ffmpeg-selftest:
	./scripts/install-ffmpeg-selftest.sh

# Proves scripts/find-browser.sh - the ONE browser resolution both CI jobs and the release
# workflow use - still refuses a browser it has not seen render. That question used to be
# asked two ways in one workflow: `build` proved a candidate could render, `dashboard`
# printed its `--version`, and the weaker one was on the job whose whole purpose is that
# the dashboard graders cannot come back green without a browser. Hermetic (every engine
# here is a shell script), so it runs on a machine with no browser at all. A guard nobody
# tries to defeat is a guard nobody knows works.
find-browser-selftest:
	./scripts/find-browser-selftest.sh

# What THIS machine's browser resolution answers, for a human debugging a red grader. It is
# the same script the workflows run, so "which engine did the gate measure" has one answer
# a contributor can also ask.
find-browser:
	@./scripts/find-browser.sh

# --- the release path (S0046) -------------------------------------------------
# Everything that decides whether the one-way door opens correctly - that a manual
# dispatch publishes nothing, that the version tag is pushed before `:latest` moves, that
# `:latest` is promoted only onto a digest that was pulled back and re-smoked - used to be
# asserted only by COMMENTS inside release.yml. That is the same "prose cannot enforce an
# invariant" failure as the ffmpeg pin, on the one path this repository has never run.
#
# The gate does not match text. It RUNS release.yml's own planning shell, once per event
# shape, and decides each step's guard from the values that run produces: flip the planning
# script so a dispatch sets publish=true and every `if:` in the file is unchanged, so a
# text matcher stays green while a dry run would push an image. Read-only; it publishes
# nothing and needs no network.
release-shape:
	go run ./scripts/release-shape-gate

# Proves the release-shape gate still BITES. Every property it asserts is defeated on
# purpose against a mutated copy of the repository, each defeat is required to be red and
# to name what it saw, and the run fails if any defeat did not execute. Deliberately NOT
# part of `check`: the mutations belong in their own target. A guard nobody tries to defeat
# is a guard nobody knows works.
release-shape-selftest:
	./scripts/release-shape-selftest.sh

# --- the dashboard (WEBUI-10) -------------------------------------------------
# internal/webui/index.html is GENERATED and COMMITTED: the binary embeds one
# self-contained file, and it is built from the modules under internal/webui/src by
# internal/webui/gen with the Go toolchain alone - no JavaScript runtime, no bundler, no
# registry package, no lockfile, no network, and so no new stage or tool in the image
# build. `webui-gen` is the only writer of that file.
webui-gen:
	go run ./internal/webui/gen/genindex

# The stale-artifact gate. A committed document that is not what the sources generate is
# a page whose behaviour nobody can predict from its source, so `check` refuses it by
# name rather than silently regenerating behind the build.
webui-stale:
	go run ./internal/webui/gen/genindex -check

# The dashboard's suites in REQUIRED mode: the derivation units (node's built-in test
# runner) and the rendered graders (a real browser engine). A runtime it needs and cannot
# find is a FAILURE here, and a suite that reported itself skipped is a failure too - the
# whole point of this target is that it cannot come back green without having measured
# anything. `check` keeps the repo's skip-when-absent idiom instead, exactly as the docker
# gate does, so a contributor with no browser is not blocked.
webui-check:
	./scripts/webui-check.sh

# The DETERMINISM gate. `webui-check` asks whether the rendered graders pass; this asks
# whether they AGREE WITH THEMSELVES - it runs them more than once against one unchanged
# tree, prints how many repetitions it ran, and fails if any two repetitions decided the
# same bytes differently. A grader that returns a different verdict on identical input is
# reporting how busy the machine was, and it cannot carry an acceptance criterion; one of
# those broke main once, and the rerun that passed is what made it look like nothing.
#
# Deliberately NOT inside `check` and not inside `webui-check`: it is the same suite run
# N times, so folding it into either would multiply the cost of every PR by N to re-ask a
# question those targets do not ask. HOLDFAST_WEBUI_REPEAT sets the count (default 3).
webui-repeat-check:
	./scripts/webui-repeat-check.sh

# Proves the dashboard's ENGINE-ONLY graders still BITE. Three questions are the whole
# reason there is a browser in this gate - the operating system's colour-scheme preference,
# the accessible name the engine computes, and the focus a real key press moves - and none
# of them is answerable from the document, so a grader that quietly stopped deciding one
# would take the coverage with it and report "ok". Each is defeated on purpose here against
# a mutated COPY of the tree, each defeat must be red AND say what it saw, and the run fails
# if any defeat did not execute. Deliberately NOT part of `check`: the mutations belong in
# their own target, and `check` must never re-render the tree it is grading. A guard nobody
# tries to defeat is a guard nobody knows works.
webui-graders-selftest:
	./scripts/webui-graders-selftest.sh

# THE gate. CI and the release workflow both run exactly this.
check: check-pins check-pins-selftest install-ffmpeg-selftest find-browser-selftest release-shape webui-stale fmt vet build test staticcheck govulncheck govulncheck-selftest

# Asks UPSTREAM whether the pinned ffmpeg release is still served. Deliberately NOT part
# of `check`: the PR gate must not red because a third party had a bad afternoon. CI runs
# this on a schedule (.github/workflows/pin-health.yml) so the next expiry reports itself
# instead of surfacing as an unexplained red job on an unrelated pull request, which is
# exactly how the last one surfaced. Needs network.
check-pin-live:
	./scripts/check-pin-live.sh

# --- packaging (TRANSCODE-9) --------------------------------------------------
# The same commands CI runs, so the packaging gate is reproducible by a human and not
# something only the runner can do.
image:
	docker buildx build --platform $(PLATFORM) --load -t $(IMAGE) \
	  --build-arg VERSION=$(VERSION) --build-arg COMMIT=$(COMMIT) --build-arg DATE=$(DATE) \
	  --build-arg SOURCE_URL=$(SOURCE_URL) .

# Builds the image, then drives a REAL oneshot encode inside it and asserts the
# no-loss contract held. This — not "the build succeeded" — is the packaging gate.
image-smoke: image
	./scripts/smoke-image.sh $(IMAGE)

compose-check:
	docker compose -f docker-compose.yml config -q && echo "docker-compose.yml is valid"

tidy:
	go mod tidy

clean:
	rm -f holdfast transcode
	rm -rf dist out
