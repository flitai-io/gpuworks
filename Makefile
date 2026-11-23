# gpuworks build orchestration.
#
# The server binary embeds the built UI (internal/server/webui/dist), so `make
# build` depends on `make ui`. All Go binaries are static (CGO_ENABLED=0).

GO        ?= go
# A caller may export GO with no value (GO= make build). `?=` only fills an
# undefined variable, so an exported-but-empty GO would reach the shell as an
# empty command and every build step would fail with "/bin/sh: N: build: not
# found" - a message that names the subcommand, never GO. Fall back instead.
ifeq ($(strip $(GO)),)
GO        := go
endif
GO_TEST_FLAGS ?=
COVERPROFILE ?=
GOBIN     := $(HOME)/.local/go/bin
export PATH := $(GOBIN):$(HOME)/go/bin:$(PATH)

VERSION   ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)
# Resolve once: link flags and the Agent manifest must name the same build.
VERSION   := $(VERSION)
COMMIT    ?= $(shell git rev-parse --short HEAD 2>/dev/null || echo none)
# DATE is the build time the binary reports. Release builds pass the pipeline's
# creation time, the same value the distribution index records, so a server
# can tell whether the channel's release is newer than itself (ADR 0040).
DATE      ?= $(shell date -u +%Y-%m-%dT%H:%M:%SZ)
DATE      := $(DATE)
# CHANNEL is the distribution channel the build follows for upgrades: nightly
# for master builds, release for v* tags (ADR 0040).
CHANNEL   ?= nightly
LDFLAGS   := -s -w \
	-X gpuworks.ai/gpuworks/internal/version.Version=$(VERSION) \
	-X gpuworks.ai/gpuworks/internal/version.Commit=$(COMMIT) \
	-X gpuworks.ai/gpuworks/internal/version.BuildDate=$(DATE) \
	-X gpuworks.ai/gpuworks/internal/version.Channel=$(CHANNEL)

# SERVER_PKG is the main package of the server binary and PACKAGE_PREFIX the
# name release-packages gives its archives (<prefix>-<release>-<os>-<arch>.tar.gz).
# A separately built server (another edition) sets both; the binary inside is
# named gpuworks either way, which is what install and upgrade unpack.
SERVER_PKG     ?= ./cmd/gpuworks
PACKAGE_PREFIX ?= gpuworks

BIN       := bin
TEST_LOG_DIR ?= .testlogs
PLATFORMS := linux/amd64 linux/arm64 darwin/arm64

# Build the lint tool with the same Go version that loads this module. A
# globally installed binary can be too old to analyze the repo's Go syntax.
GOLANGCI_LINT_VERSION := v1.64.8
LINT_GO_VERSION := $(shell $(GO) env GOVERSION)
LINT_BIN := $(CURDIR)/$(BIN)/tools/golangci-lint-$(GOLANGCI_LINT_VERSION)-$(LINT_GO_VERSION)/golangci-lint

.PHONY: all
all: build

.PHONY: docs-pdf
## docs-pdf: build the bilingual site and print validated, versioned user handbooks
docs-pdf: check-doc-links check-translations
	bash docs-site/build.sh
	python3 docs-site/pdf_browser_checks.py
	python3 docs-site/export_pdf.py --version "$(VERSION)" --commit "$(COMMIT)"

.PHONY: build
## build: full release build — cross-compile agents (embedded) + UI + server + agent
build: check-libvgpu ui cross-agents server agent

## agent-image: the agent image a Kubernetes cluster's DaemonSet runs, built
## into the local docker (AGENT_IMAGE, default gpuworks/gpuworks-agent:<revision
## in build/agent-image/REVISION>; ARCH amd64|arm64). agent-image-publish builds amd64 + arm64 and pushes them to
## GPUWORKS_AGENT_IMAGE_REPOS (the release pipeline's release-agent-image job).
AGENT_IMAGE ?= gpuworks/gpuworks-agent:$(shell tr -d '[:space:]' < build/agent-image/REVISION)
NERDCTL_VERSION ?= 2.1.6
ARCH ?= amd64
DEBIAN_MIRROR ?= mirrors.aliyun.com
.PHONY: agent-image
agent-image:
	GO="$(GO)" LDFLAGS="$(LDFLAGS)" VERSION="$(VERSION)" ARCH="$(ARCH)" AGENT_IMAGE="$(AGENT_IMAGE)" \
	NERDCTL_VERSION="$(NERDCTL_VERSION)" DEBIAN_MIRROR="$(DEBIAN_MIRROR)" ./scripts/agent_image.sh build

.PHONY: agent-image-publish
agent-image-publish:
	GO="$(GO)" LDFLAGS="$(LDFLAGS)" VERSION="$(VERSION)" \
	NERDCTL_VERSION="$(NERDCTL_VERSION)" DEBIAN_MIRROR="$(DEBIAN_MIRROR)" ./scripts/agent_image.sh publish

.PHONY: server
## server: build the control-plane binary (embeds UI + cross-compiled agents)
server: cross-agents
	CGO_ENABLED=0 $(GO) build -buildvcs=false -trimpath -ldflags "$(LDFLAGS)" -o $(BIN)/gpuworks $(SERVER_PKG)

.PHONY: server-linux-amd64
# Explicit release targets avoid accidentally deploying a host (for example,
# Darwin/arm64) control-plane binary to a Linux node from a developer laptop.
server-linux-amd64: ui cross-agents
	CGO_ENABLED=0 GOOS=linux GOARCH=amd64 $(GO) build -buildvcs=false -trimpath -ldflags "$(LDFLAGS)" -o $(BIN)/gpuworks-linux-amd64 $(SERVER_PKG)

.PHONY: server-linux-amd64-prebuilt-ui
# CI receives internal/server/webui/dist from core-ui. This target builds the
# hosted cloud-test server without trying to run npm inside the Go image.
server-linux-amd64-prebuilt-ui: cross-agents
	@test -f internal/server/webui/dist/index.html || (echo "prebuilt UI artifact is required" >&2; exit 2)
	CGO_ENABLED=0 GOOS=linux GOARCH=amd64 $(GO) build -buildvcs=false -trimpath -ldflags "$(LDFLAGS)" -o $(BIN)/gpuworks-linux-amd64 $(SERVER_PKG)

.PHONY: server-linux-arm64
server-linux-arm64: ui cross-agents
	CGO_ENABLED=0 GOOS=linux GOARCH=arm64 $(GO) build -buildvcs=false -trimpath -ldflags "$(LDFLAGS)" -o $(BIN)/gpuworks-linux-arm64 $(SERVER_PKG)

.PHONY: server-linux-arm64-prebuilt-ui
# CI receives the same prebuilt UI artifact for both server architectures.
server-linux-arm64-prebuilt-ui: cross-agents
	@test -f internal/server/webui/dist/index.html || (echo "prebuilt UI artifact is required" >&2; exit 2)
	CGO_ENABLED=0 GOOS=linux GOARCH=arm64 $(GO) build -buildvcs=false -trimpath -ldflags "$(LDFLAGS)" -o $(BIN)/gpuworks-linux-arm64 $(SERVER_PKG)

.PHONY: server-darwin-amd64-prebuilt-ui
# macOS release binaries are cross-compiled by the Linux release job. They
# still embed Linux agents because managed GPU nodes run the agent on Linux.
server-darwin-amd64-prebuilt-ui: cross-agents
	@test -f internal/server/webui/dist/index.html || (echo "prebuilt UI artifact is required" >&2; exit 2)
	CGO_ENABLED=0 GOOS=darwin GOARCH=amd64 $(GO) build -buildvcs=false -trimpath -ldflags "$(LDFLAGS)" -o $(BIN)/gpuworks-darwin-amd64 $(SERVER_PKG)

.PHONY: server-darwin-arm64-prebuilt-ui
server-darwin-arm64-prebuilt-ui: cross-agents
	@test -f internal/server/webui/dist/index.html || (echo "prebuilt UI artifact is required" >&2; exit 2)
	CGO_ENABLED=0 GOOS=darwin GOARCH=arm64 $(GO) build -buildvcs=false -trimpath -ldflags "$(LDFLAGS)" -o $(BIN)/gpuworks-darwin-arm64 $(SERVER_PKG)

.PHONY: release-packages
## release-packages: build versioned Linux and macOS server archives with checksums
release-packages:
	./scripts/ui_overlay_guard.sh
	GPUWORKS_RELEASE_CHANNEL="$${GPUWORKS_RELEASE_CHANNEL:-$(CHANNEL)}" GPUWORKS_SERVER_PKG="$(SERVER_PKG)" GPUWORKS_PACKAGE_PREFIX="$(PACKAGE_PREFIX)" ./scripts/package_release.sh

DIST_DIR ?= dist

.PHONY: dist-publish
## dist-publish: sign dist/ and the embedded catalogs into the CHANNEL tree and publish it to every distribution source
dist-publish:
	./scripts/publish_dist.sh $(CHANNEL) $(DIST_DIR)

.PHONY: agent
## agent: build the node agent binary
agent: check-libvgpu
	CGO_ENABLED=0 $(GO) build -buildvcs=false -trimpath -ldflags "$(LDFLAGS)" -o $(BIN)/gpuworks-agent ./cmd/gpuworks-agent

AGENTBIN := internal/server/agentbin/dl

## cross-agents: cross-compile agents into $(BIN)/dl and embed each in the server
## zstd-compressed (so the single server binary serves them at /dl/ for curl|sh
## install — no download host). The zstd CLI at -19 packs about 10% smaller
## than the Go encoder (scripts/zstdpack), which is the fallback without it;
## the server reads either.
cross-agents: check-libvgpu
	@mkdir -p $(AGENTBIN) $(BIN)/dl
	@rm -f $(AGENTBIN)/gpuworks-agent-* $(AGENTBIN)/agent-manifest.json
	@for p in $(PLATFORMS); do \
		os=$${p%/*}; arch=$${p#*/}; name=gpuworks-agent-$$os-$$arch; \
		echo "  embed agent $$os/$$arch"; \
		CGO_ENABLED=0 GOOS=$$os GOARCH=$$arch $(GO) build -buildvcs=false -trimpath -ldflags "$(LDFLAGS)" \
			-o $(BIN)/dl/$$name ./cmd/gpuworks-agent || exit 1; \
		if command -v zstd >/dev/null 2>&1; then \
			zstd -19 -q -f -o $(AGENTBIN)/$$name.zst $(BIN)/dl/$$name || exit 1; \
		else \
			CGO_ENABLED=0 $(GO) run ./scripts/zstdpack -o $(AGENTBIN)/$$name.zst $(BIN)/dl/$$name || exit 1; \
		fi; \
	done
	CGO_ENABLED=0 $(GO) run ./scripts/agentmanifest -version "$(VERSION)" -o $(AGENTBIN)/agent-manifest.json $(addprefix $(BIN)/dl/gpuworks-agent-,$(subst /,-,$(PLATFORMS)))

.PHONY: cross
## cross: alias kept for the demo (the plain agents are in $(BIN)/dl)
cross: cross-agents

.PHONY: ui
## ui: install or refresh deps when package-lock.json changed (ui/ensure-deps.mjs), then build the frontend into the embed dir
ui: i18n-check
	./scripts/check_ui_architecture.sh
	./scripts/ui_overlay_guard.sh
	@if [ -f secrets/i18n-llm.env ]; then echo "  i18n LLM config detected; run 'make i18n-translate' to fill new missing keys"; fi
	@cd ui && node ensure-deps.mjs
	cd ui && npm run lint:i18n
	cd ui && npm run build
	./scripts/check_ui_bundle_size.sh

.PHONY: i18n-check
i18n-check:
	node scripts/i18n-check.mjs

.PHONY: i18n-translate
## i18n-translate: fill missing target-locale keys through the optional local translator
i18n-translate:
	node scripts/i18n-pipeline.mjs translate

.PHONY: i18n-review
## i18n-review: review changed translations and write review-state/report; never rewrites locale text
i18n-review:
	node scripts/i18n-pipeline.mjs review

## test: build the embedded UI/assets and run the complete untagged Go suite
## UNIT_TIMEOUT replaces `go test`'s 10m default, which the untagged suite has
## outgrown on its slowest package: `internal/server/api` measured 433s and
## then 600s on the same host, so the default killed a suite that was still
## passing and reported whichever case was in flight. A real hang still fails
## here, well inside the job's own limit. (`-race` needs much more again, see
## known-issues T54.)
UNIT_TIMEOUT ?= 30m

.PHONY: test-unit
## test-unit: build the embedded UI first, then run the untagged Go suite.
## `go test ./...` cannot compile webui without the generated embed tree.
test-unit: check-change-record
	$(MAKE) test-unit-checked

# A recursive boundary keeps even make -j from starting UI or tests before
# the change-record gate succeeds.
.PHONY: test-unit-checked
test-unit-checked: ui check-time-policy check-architecture check-api-conventions check-ci-yaml check-artifacts githooks check-doc-links check-translations test-host-env-adapters test-e2e-coverage-check test-inference-template-assets test-strata-queue-assets test-sglang-video-cancel-assets test-runner-image-scripts
	CGO_ENABLED=0 $(GO) test -timeout $(UNIT_TIMEOUT) $(GO_TEST_FLAGS) ./...
	cd build/runtime-assets/hygon-vdev-helper && CGO_ENABLED=0 $(GO) test $(GO_TEST_FLAGS) ./...
	$(MAKE) test-extapi
	$(MAKE) test-pluginsdk
	$(MAKE) check-tagged-build

## test-extapi: gofmt, vet and test pkg/extapi, the plugin wire formats. It is
## a module of its own, which `go test ./...` and `go vet ./...` at the root
## do not enter; test-unit, vet and both CI pipelines run it.
.PHONY: test-extapi
test-extapi:
	@unformatted=$$(cd pkg/extapi && gofmt -l .); if [ -n "$$unformatted" ]; then echo "pkg/extapi needs gofmt: $$unformatted" >&2; exit 1; fi
	cd pkg/extapi && CGO_ENABLED=0 $(GO) vet ./...
	cd pkg/extapi && CGO_ENABLED=0 $(GO) test $(GO_TEST_FLAGS) ./...

## test-pluginsdk: gofmt, vet and test pkg/pluginsdk, what plugins are built
## with. Like pkg/extapi it is a module of its own that the root's
## `go test ./...` and `go vet ./...` do not enter.
.PHONY: test-pluginsdk
test-pluginsdk:
	@unformatted=$$(cd pkg/pluginsdk && gofmt -l .); if [ -n "$$unformatted" ]; then echo "pkg/pluginsdk needs gofmt: $$unformatted" >&2; exit 1; fi
	cd pkg/pluginsdk && CGO_ENABLED=0 $(GO) vet ./...
	cd pkg/pluginsdk && CGO_ENABLED=0 $(GO) test $(GO_TEST_FLAGS) ./...

## test-plugin-landlock: require real sandbox denials, each named PASS and no
## skipped cases. Keep environment and test logs for both passing and failed CI.
PLUGIN_LANDLOCK_CASES := TestContractLandlockKeepsThePluginFromTheMasterKey TestLandlockDeniesPathsOutsideThePlugin TestSiblingCannotReachAnotherPluginsSockets TestDeviceDirGrantsNodesCreatedLater
.PHONY: test-plugin-landlock
test-plugin-landlock:
	@mkdir -p .testlogs
	@{ uname -sr; \
		if [ -r /proc/self/status ]; then grep -E '^(NoNewPrivs|Seccomp|Seccomp_filters):' /proc/self/status || true; else echo '/proc/self/status unavailable'; fi; \
		if [ -r /sys/kernel/security/lsm ]; then cat /sys/kernel/security/lsm; printf '\n'; else echo 'LSM list unavailable'; fi; \
	} > .testlogs/landlock-environment.log
	@cat .testlogs/landlock-environment.log
	@GPUWORKS_TEST_STRICT=1 CGO_ENABLED=0 $(GO) test -v -count=1 -timeout 10m \
		-run "^($$(echo $(PLUGIN_LANDLOCK_CASES) | tr ' ' '|'))$$" ./internal/testenv/extplugin/ \
		> .testlogs/landlock-strict.log 2>&1 || { tail -n 80 .testlogs/landlock-strict.log; exit 1; }
	@for c in $(PLUGIN_LANDLOCK_CASES); do \
		grep -q -- "^--- PASS: $$c " .testlogs/landlock-strict.log || { echo "$$c did not pass"; tail -n 80 .testlogs/landlock-strict.log; exit 1; }; \
	done
	@if grep -q -- '--- SKIP' .testlogs/landlock-strict.log; then grep -- '--- SKIP' .testlogs/landlock-strict.log; exit 1; fi
	@grep -E -- '--- (PASS|SKIP|FAIL)' .testlogs/landlock-strict.log

.PHONY: test-host-env-adapters
## test-host-env-adapters: the serve adapters macOS environment packages ship
## (build/host-envs/adapters); standard library only, so no install step
test-host-env-adapters:
	cd build/host-envs/adapters && python3 -B -m unittest discover -p 'test_*.py'

.PHONY: test-runner-image-scripts
test-runner-image-scripts:
	python3 -B build/runner-images/test_prebuilt.py
	python3 -B build/runner-images/test_ascend_reload_patch.py

.PHONY: test-inference-template-assets
test-inference-template-assets:
	python3 -B -m unittest discover -s build/inference-templates/glm53-910c -p 'test_*.py'
	python3 -B -m unittest discover -s build/inference-templates/glm53-910b -p 'test_*.py'
	python3 -B -m unittest discover -s test/runtime-wrapper-assets -p '*_test.py'

.PHONY: test-strata-queue-assets
test-strata-queue-assets:
	python3 -B build/strata-queue/test_strata_queue.py
	python3 -B build/strata-queue/build.py --check

.PHONY: test-sglang-video-cancel-assets
test-sglang-video-cancel-assets:
	python3 -B build/sglang-video-cancel/test_install.py
	python3 -B build/sglang-video-cancel/build.py --check

## test-dqlite-engine: the clustered engine's tests (-tags=dqlite,nosqlite3).
## In that build every test that opens the store starts the database process,
## which this repository does not build: GPUWORKS_DQLITE_BIN must name the
## gpuworks-dqlite executable of a build that ships the cluster runtime.
## Without it the suite is skipped, or fails under GPUWORKS_TEST_STRICT=1.
## DQLITE_ENGINE_PKGS narrows the packages.
DQLITE_ENGINE_PKGS ?= ./internal/server/... ./internal/testenv/...
.PHONY: test-dqlite-engine
test-dqlite-engine:
	@if [ -n "$$GPUWORKS_DQLITE_BIN" ] && [ -f "$$GPUWORKS_DQLITE_BIN" ] && [ -x "$$GPUWORKS_DQLITE_BIN" ]; then \
		CGO_ENABLED=0 $(GO) test -timeout $(UNIT_TIMEOUT) $(GO_TEST_FLAGS) -count=1 -tags='dqlite,nosqlite3' $(DQLITE_ENGINE_PKGS); \
	elif [ "$$GPUWORKS_TEST_STRICT" = 1 ]; then \
		echo "test-dqlite-engine: GPUWORKS_DQLITE_BIN must name the gpuworks-dqlite executable" >&2; exit 1; \
	else \
		echo "test-dqlite-engine: SKIP, GPUWORKS_DQLITE_BIN does not name the gpuworks-dqlite executable; no clustered engine test ran"; \
	fi

.PHONY: check-tagged-build
## check-tagged-build: compile every build-tag suite without running it, so a
## refactor cannot break an opt-in lane while the untagged suite stays green
## (core-test runs this too)
check-tagged-build:
	CGO_ENABLED=0 $(GO) test -tags='multiproc' ./test/l2 -run '^$$'
	CGO_ENABLED=0 $(GO) vet -tags='dqlite,nosqlite3' ./internal/server/... ./internal/testenv/...
	CGO_ENABLED=0 $(GO) test -tags='docker' ./test/l3 -run '^$$'
	CGO_ENABLED=0 $(GO) test -tags='kind' ./test/l3kind -run '^$$'
	CGO_ENABLED=0 $(GO) test -tags='cloudgpu,runpod' ./test/l4 -run '^$$'
	CGO_ENABLED=0 $(GO) test -tags='cloudgpu,compshare' ./test/l4 -run '^$$'
	CGO_ENABLED=0 $(GO) test -tags='cloudgpu,aliyun' ./test/l4 -run '^$$'
	CGO_ENABLED=0 $(GO) test -tags='cloudgpu,autodl' ./test/l4 -run '^$$'
	CGO_ENABLED=0 $(GO) test -tags='cloudgpu,vast' ./test/l4 -run '^$$'
	CGO_ENABLED=0 $(GO) test -tags='cloudgpu,partition' ./test/l4 -run '^$$'

# Keep wall-time UI budgets from measuring contention between test files.
# Override with e.g. make test-ui UI_TEST_CONCURRENCY=4.
UI_TEST_CONCURRENCY ?= $(shell node -p 'Math.max(1, Math.min(8, require("node:os").availableParallelism() - 1))')

.PHONY: test-ui
test-ui:
	cd ui && UI_TEST_CONCURRENCY=$(UI_TEST_CONCURRENCY) npm test

.PHONY: test-ui-motion
## Real Chromium layout regression; requires Python Playwright and UI dependencies.
test-ui-motion:
	python3 ui/tests/browser/reduced_motion.py

.PHONY: test-ui-playground-layout
## Real Chromium component geometry; no live server or GPU required.
test-ui-playground-layout:
	python3 -B ui/tests/browser/playground_layout.py

.PHONY: test-ui-agent-upgrade-results-layout
## Real Chromium result-column geometry; requires no management server or GPU.
test-ui-agent-upgrade-results-layout:
	python3 -B ui/tests/browser/agent_upgrade_results_layout.py

.PHONY: test-ui-template-catalog-layout
## Real Chromium catalog geometry at 1280/1920, both themes; build UI first.
UI_BROWSER_PYTHON ?= python3
test-ui-template-catalog-layout:
	$(UI_BROWSER_PYTHON) -B ui/tests/browser/template_catalog_layout.py $(UI_TEMPLATE_CATALOG_TESTS)

.PHONY: test-ui-storage-image-layout
## Real Chromium storage/image drawer geometry with production CSS, both themes.
test-ui-storage-image-layout:
	python3 -B ui/tests/browser/storage_image_layout.py

.PHONY: ensure-bins
## ensure-bins: the E2E entry points need bin/gpuworks and bin/gpuworks-agent,
## but a full `make build` (UI bundle plus every cross-compiled agent) takes
## minutes even when nothing changed. Build only when a binary is missing, so
## re-running one case does not pay for a rebuild it does not need.
ensure-bins:
	@if [ -x bin/gpuworks ] && [ -x bin/gpuworks-agent ]; then \
		echo "  reusing existing bin/gpuworks and bin/gpuworks-agent"; \
	else \
		echo "  binaries missing; running make build"; \
		$(MAKE) build || exit 1; \
	fi

.PHONY: test-e2e-setup-check
## test-e2e-setup-check: report which E2E prerequisites this machine is missing.
## Runs no test and never fails on a missing dependency; it only tells you what
## to install next. See test/e2e/RUNNING.md for the four dependency layers.
test-e2e-setup-check:
	@missing=0; \
	command -v python3 >/dev/null || { echo "MISSING python3"; missing=1; }; \
	python3 -c "import playwright" 2>/dev/null || { echo "MISSING playwright  -> python3 -m pip install -r requirements-e2e.txt"; missing=1; }; \
	{ google-chrome --version || chromium --version; } >/dev/null 2>&1 || { echo "MISSING Google Chrome (E2E uses channel=\"chrome\")"; missing=1; }; \
	command -v objdump >/dev/null || { echo "MISSING objdump  -> apt install binutils (needed by make check-libvgpu)"; missing=1; }; \
	test -f bin/gpuworks || { echo "MISSING bin/gpuworks  -> run make build"; missing=1; }; \
	test -f bin/gpuworks-agent || { echo "MISSING bin/gpuworks-agent  -> run make build"; missing=1; }; \
	docker info >/dev/null 2>&1 || echo "NOTE no reachable Docker daemon: only the Docker-class cases need it"; \
	if [ $$missing -eq 0 ]; then echo "E2E prerequisites look complete: make test-e2e-smoke"; else echo "Fix the MISSING lines above, then run make test-e2e-smoke"; fi

.PHONY: test-e2e-docker-check
## test-e2e-docker-check: report whether the Docker-class cases can run here.
## Like test-e2e-setup-check this runs no test; it names the fix for each gap.
test-e2e-docker-check:
	@ok=0; \
	if docker info >/dev/null 2>&1; then echo "docker daemon: reachable"; else echo "MISSING docker daemon  -> start Docker, or set DOCKER_HOST to a reachable socket"; ok=1; fi; \
	if docker image inspect gpuworks/fake-engine:test >/dev/null 2>&1; then echo "engine image: gpuworks/fake-engine:test present"; else echo "MISSING engine image  -> make fake-engine-image"; ok=1; fi; \
	if [ $$ok -eq 0 ]; then echo "Docker-class cases can run: make test-e2e-playground-chat"; else echo "Fix the MISSING lines above; see test/e2e/RUNNING.md"; fi

.PHONY: test-e2e-playground-chat
## test-e2e-playground-chat: playground streaming, conversations, preview and
## compare. Needs the Docker daemon and the fake-engine image (make fake-engine-image).
test-e2e-playground-chat: ensure-bins
	NO_PROXY=127.0.0.1,localhost no_proxy=127.0.0.1,localhost python3 test/e2e/playground_chat_gateway.py --locale en

.PHONY: test-e2e-smoke
## test-e2e-smoke: the shortest real E2E path - Chrome, an isolated server and
## SQLite, no Docker and no GPU. Use this to prove the setup works before
## running narrower cases.
test-e2e-smoke: ensure-bins
	NO_PROXY=127.0.0.1,localhost no_proxy=127.0.0.1,localhost python3 test/e2e/platform_token_read_revoke.py
	NO_PROXY=127.0.0.1,localhost no_proxy=127.0.0.1,localhost python3 test/e2e/session_logout.py

.PHONY: test-e2e-platform-token
## test-e2e-platform-token: real Chrome + isolated server/SQLite; no GPU required
test-e2e-platform-token: ensure-bins
	NO_PROXY=127.0.0.1,localhost no_proxy=127.0.0.1,localhost python3 test/e2e/platform_token_read_revoke.py

.PHONY: test-e2e-inference-key test-e2e-network-storage test-e2e-mcp-settings test-e2e-local
## Local browser closures: isolated real server/SQLite and Chrome, no GPU.
test-e2e-inference-key: ensure-bins
	NO_PROXY=127.0.0.1,localhost no_proxy=127.0.0.1,localhost python3 test/e2e/inference_key_lifecycle.py

test-e2e-network-storage: ensure-bins
	NO_PROXY=127.0.0.1,localhost no_proxy=127.0.0.1,localhost python3 test/e2e/network_storage_lifecycle.py

test-e2e-mcp-settings: ensure-bins
	NO_PROXY=127.0.0.1,localhost no_proxy=127.0.0.1,localhost python3 test/e2e/mcp_settings_lifecycle.py

## Build once, then execute each independent case; any failure stops this target.
test-e2e-local: ensure-bins
	NO_PROXY=127.0.0.1,localhost no_proxy=127.0.0.1,localhost python3 test/e2e/provider_endpoint_defaults.py
	NO_PROXY=127.0.0.1,localhost no_proxy=127.0.0.1,localhost python3 test/e2e/image_catalog.py --case register
	NO_PROXY=127.0.0.1,localhost no_proxy=127.0.0.1,localhost python3 test/e2e/image_catalog.py --case delete-version
	NO_PROXY=127.0.0.1,localhost no_proxy=127.0.0.1,localhost python3 test/e2e/platform_token_read_revoke.py
	NO_PROXY=127.0.0.1,localhost no_proxy=127.0.0.1,localhost python3 test/e2e/inference_key_lifecycle.py
	NO_PROXY=127.0.0.1,localhost no_proxy=127.0.0.1,localhost python3 test/e2e/network_storage_lifecycle.py
	NO_PROXY=127.0.0.1,localhost no_proxy=127.0.0.1,localhost python3 test/e2e/mcp_settings_lifecycle.py
	NO_PROXY=127.0.0.1,localhost no_proxy=127.0.0.1,localhost python3 test/e2e/session_logout.py
	NO_PROXY=127.0.0.1,localhost no_proxy=127.0.0.1,localhost python3 test/e2e/provider_endpoint_edit.py

.PHONY: test-fleet-perf-processes
## test-fleet-perf-processes: the same control-plane load case with the server
## as its own process and the fleet as a second one, so each side's memory and
## CPU can be read separately. Node count is tunable: GPUWORKS_FLEET_NODES /
## GPUWORKS_FLEET_GPUS (cards per node, default 1) /
## GPUWORKS_FLEET_CHURN_NODES / GPUWORKS_FLEET_ROUNDS.
## GPUWORKS_FLEET_SERVER_BIN / GPUWORKS_FLEET_HOST_BIN reuse prebuilt binaries,
## which is how it runs on a machine with no Go toolchain.
test-fleet-perf-processes:
	GPUWORKS_TEST_PERF=1 CGO_ENABLED=0 $(GO) test -count=1 -v -tags=multiproc -timeout 60m ./test/l2/ -run 'TestFleet1000NodesRedeployChurnAcrossProcesses'

.PHONY: test-fleet-log-scale-processes
## test-fleet-log-scale-processes: the log-center read targets under fleet load
## (agents streaming instance logs while half the nodes redeploy), with the
## server as its own process and the fleet as a second one. Scaling variables
## as test-fleet-perf-processes, plus GPUWORKS_LOG_LINES_PER_INSTANCE_PER_SEC
## (default 2) and GPUWORKS_LOG_SAMPLE_SECONDS (default 90).
test-fleet-log-scale-processes:
	GPUWORKS_TEST_PERF=1 CGO_ENABLED=0 $(GO) test -count=1 -v -tags=multiproc -timeout 60m ./test/l2/ -run 'TestFleetLogCenterScaleAcrossProcesses'

.PHONY: test-fleet-benchmark-processes
## test-fleet-benchmark-processes: a server-origin saturation evaluation
## (300 sessions of 1024 streamed tokens through the gateway) with the server
## as its own process and the fleet as a second one; asserts the server's RSS
## budget and operator list reads while the server drives the load. Shape and
## budgets: GPUWORKS_BENCH_* (see the test's doc comment).
test-fleet-benchmark-processes:
	GPUWORKS_TEST_PERF=1 CGO_ENABLED=0 $(GO) test -count=1 -v -tags=multiproc -timeout 60m ./test/l2/ -run '^TestFleetServerOriginBenchmarkAcrossProcesses$$'

## test-fleet-gateway-load-processes: the /v1 data plane under load - the
## server, a fleet of real agents with streaming fake engines, and a client
## population, each its own process - at the nightly shape (400 instances,
## 1000 streaming clients for 30s unless GPUWORKS_GATEWAY_CONCURRENCY /
## GPUWORKS_GATEWAY_SECONDS say otherwise). Asserts errors, TTFT overhead,
## replica spread and, when GPUWORKS_GATEWAY_MAX_SERVER_CPU_US_PER_REQUEST is
## set, the server's CPU per request. Other knobs: GPUWORKS_GATEWAY_* (see the
## test's doc comment).
GATEWAY_LOAD_CONCURRENCY ?= 1000
GATEWAY_LOAD_SECONDS ?= 30
.PHONY: test-fleet-gateway-load-processes
test-fleet-gateway-load-processes:
	GPUWORKS_TEST_PERF=1 GPUWORKS_GATEWAY_CONCURRENCY=$(GATEWAY_LOAD_CONCURRENCY) GPUWORKS_GATEWAY_SECONDS=$(GATEWAY_LOAD_SECONDS) CGO_ENABLED=0 $(GO) test -count=1 -v -tags=multiproc -timeout 40m ./test/l2/ -run '^TestFleetGatewayLoadAcrossProcesses$$'

.PHONY: test-integration
## test-integration: fast product-level tests that use fake agents/runtimes.
test-integration: ui
	CGO_ENABLED=0 $(GO) test -timeout $(UNIT_TIMEOUT) $(GO_TEST_FLAGS) ./internal/integrationtest/... ./internal/testenv/...

.PHONY: test-coverage-report
## test-coverage-report: emit a machine-readable coverage profile for the
## untagged suite. Tagged suites have separate CI jobs and reports.
test-coverage-report: ui
	CGO_ENABLED=0 $(GO) test -coverprofile=coverage.out ./...

.PHONY: test
test: check-change-record
	$(MAKE) test-unit-checked test-ui

.PHONY: check-time-policy
check-time-policy:
	./scripts/check_time_policy.sh

.PHONY: check-architecture
## check-architecture: enforce scripts/arch-constraints.txt (ratcheting grep/go list
## budgets) and the package dependency-direction rules. See scripts/arch-constraints.md.
check-architecture:
	./scripts/check_architecture.sh

## check-change-record: a contract or decision path in the diff needs a record.
## Contract means docs/ in the same diff. Decision means a same-named
## gpuworks-artifacts branch whose plan.md has decision and rejected.
## The path list is scripts/change-record-rules.txt. No N/A flag.
.PHONY: check-change-record
check-change-record:
	CGO_ENABLED=0 $(GO) run ./scripts/changerecord

## code-health-report: where the tree got harder to change since
## CODE_HEALTH_SINCE: churn, per-function complexity, deferrals
## (scripts/codehealth). A report for comparing periods; it gates nothing.
CODE_HEALTH_SINCE ?= 7 days ago
CODE_HEALTH_HEAD ?= HEAD
.PHONY: code-health-report
code-health-report:
	@CGO_ENABLED=0 $(GO) run ./scripts/codehealth -since "$(CODE_HEALTH_SINCE)" -head "$(CODE_HEALTH_HEAD)"

.PHONY: check-api-conventions
## check-api-conventions: response envelopes and snake_case field names in
## internal/server/api, read off the types (docs/reference/api-conventions.md).
check-api-conventions:
	CGO_ENABLED=0 $(GO) run ./scripts/apiconv

.PHONY: check-ci-yaml
## check-ci-yaml: reject '#' comments inside folded (>) scalars, which become
## shell comments and truncate the assembled command, and needs that name a
## job of the same or a later stage (GitLab 14.0 refuses the pipeline).
## Every build/test job must also wait for the mandatory change-record gate.
check-ci-yaml:
	./scripts/check_ci_yaml.sh
	python3 -B -m unittest discover -s scripts -p test_check_ci_yaml.py

.PHONY: check-artifacts
## check-artifacts: verify plan-doc frontmatter (status/dates/summary) across
## artifacts/{work,archive,research}, that every topic directory has exactly
## the required entry file with the right status, that artifacts/README.md,
## artifacts/.ignore and plan.md's generated block match it, and that every
## artifacts/... reference in the repo resolves to a real path.
check-artifacts: githooks
	CGO_ENABLED=0 $(GO) run ./scripts/artifactsindex

.PHONY: githooks
## githooks: point git at scripts/githooks (new worktrees then get the plans
## checkout too). Leaves an existing core.hooksPath alone, and skips a clone
## whose .git/hooks holds hooks of its own, which the setting would disable;
## silent when there is nothing to do (CI, a tarball, no git).
githooks:
	@[ -z "$$CI" ] && git rev-parse --git-dir >/dev/null 2>&1 \
	  && [ -z "$$(git config --get core.hooksPath)" ] \
	  && ! ls "$$(git rev-parse --git-common-dir)/hooks" 2>/dev/null | grep -v -e '\.sample$$' -e '^post-checkout$$' | grep -q . \
	  && git config core.hooksPath scripts/githooks || true

.PHONY: check-doc-links
## check-doc-links: verify relative Markdown links (and #anchors) in docs/, README*.md,
## CONTEXT.md and CLAUDE.md, and reject [[wiki]] references under docs/.
check-doc-links:
	CGO_ENABLED=0 $(GO) run ./scripts/doclinks

.PHONY: check-translations
## check-translations: every section of a docs-en/ page must match the reviewed
## SHA-256 of its docs/ source section recorded at the end of the page, with the same headings and
## code blocks (the same check the docs-build CI job runs, without Hugo)
check-translations:
	python3 -B docs-site/check_translations.py

## api-docs: rebuild what is derived from docs/reference/api.md and its English
## page: the source hashes at the end of docs-en/reference/api.md and the AI
## assistant's route catalog internal/server/aihelper/apidocs_gen.go. Neither is
## merged by hand: when a merge or rebase conflicts in them, settle the text of
## the two pages, run this, and `git add` the results. It refuses while either
## page's text still has conflict lines, and starts the catalog from empty when
## the merge left it uncompilable.
API_ROUTE_DOCS := internal/server/aihelper/apidocs_gen.go
.PHONY: api-docs
api-docs:
	python3 -B docs-site/check_translations.py --write docs/reference/api.md >/dev/null
	@CGO_ENABLED=0 $(GO) build ./internal/server/aihelper 2>/dev/null \
	  || printf 'package aihelper\n\nvar routeDocs = map[string]string{}\n' > $(API_ROUTE_DOCS)
	CGO_ENABLED=0 $(GO) test ./internal/server/aihelper -count=1 -run '^TestRouteDocsMatchAPIReference$$' -update
	CGO_ENABLED=0 $(GO) test ./internal/server/aihelper -count=1 -run '^TestRouteDoc'

.PHONY: check-assets
## check-assets: validate embedded native assets independently of Go tests
check-assets: check-libvgpu check-perftest check-ascend-compat

.PHONY: check-ascend-compat
check-ascend-compat:
	./scripts/check_ascend_compat.sh

.PHONY: runtime-assets
## runtime-assets: reproducibly rebuild the full partition runtime/probe matrix
runtime-assets:
	./build/runtime-assets/build-all.sh

.PHONY: runtime-assets-probe
## runtime-assets-probe: rebuild both embedded probe-image architectures
runtime-assets-probe:
	PROBE_GOARCH=amd64 ./build/runtime-assets/probe/build-probe-image.sh
	PROBE_GOARCH=arm64 ./build/runtime-assets/probe/build-probe-image.sh

.PHONY: runtime-assets-verify
## runtime-assets-verify: validate matrix completeness and every embedded hash
runtime-assets-verify:
	./build/runtime-assets/verify.sh

IMAGE_MATRIX ?= build/runner-images/matrix.yaml
IMAGE_MANIFEST_SEED := internal/server/imagecatalog/catalog-seed.json

.PHONY: image-manifest
## image-manifest: regenerate the system image catalog seed from the gpuworks-runner matrix
image-manifest:
	$(GO) run ./cmd/gpuworks-image-manifest -in $(IMAGE_MATRIX) -out $(IMAGE_MANIFEST_SEED) $(if $(IMAGE_MANIFEST_DATE),-date $(IMAGE_MANIFEST_DATE))

.PHONY: image-manifest-verify
## image-manifest-verify: check every reference in the seed pulls whole from every registry it lists
image-manifest-verify:
	$(GO) run ./cmd/gpuworks-image-manifest -verify $(IMAGE_MANIFEST_SEED)

.PHONY: fake-engine
fake-engine:
	@arch="$${FAKE_ENGINE_GOARCH:-$$(docker info --format '{{.Architecture}}' 2>/dev/null)}"; \
	[ -n "$$arch" ] || arch="$$(uname -m)"; \
	arch="$$(printf '%s' "$$arch" | sed 's/aarch64/arm64/;s/x86_64/amd64/')"; \
	CGO_ENABLED=0 GOOS=linux GOARCH="$$arch" $(GO) build -buildvcs=false -trimpath -o test/fake-engine/fake-engine ./test/fake-engine
	@ln -f test/fake-engine/fake-engine test/fake-engine/vllm
	@ln -f test/fake-engine/fake-engine test/fake-engine/sglang
	@tar -cf test/fake-engine/aliases.tar -C test/fake-engine fake-engine vllm sglang

.PHONY: fake-engine-image
fake-engine-image: fake-engine
	docker build -t gpuworks/fake-engine:test test/fake-engine
	docker save -o test/fake-engine/fake-engine.tar gpuworks/fake-engine:test

.PHONY: fake-model
fake-model:
	$(GO) run ./test/fake-model --out test/fake-model/testdata/qwen3-4b-fake

## test-l2: multi-process L2 suite. The full `go test` output is streamed and
## also kept in $(TEST_LOG_DIR)/l2.log: the suite takes minutes and has failed
## in full regressions with nothing left but the package line, which named no
## case (todo T29). The log path is printed on failure.
##
## L2_TIMEOUT replaces `go test`'s 10m default, which this suite had outgrown:
## it runs 440-460s locally and crossed 600s on a CI runner under coverage
## instrumentation, where the package-level timeout panic reports whichever
## case happened to be in flight rather than anything that went wrong. The
## limit stays well inside the job's own 45m so a real hang still fails here.
L2_TIMEOUT ?= 30m

## L2_SHARD=<index>/<total> runs one round-robin share of the suite's
## top-level tests (scripts/go_test_shard.sh); CI splits the suite this way.
L2_SHARD ?=

.PHONY: test-l2
test-l2:
	@set -eu; \
	mkdir -p $(TEST_LOG_DIR); \
	run_flag=""; \
	if [ -n "$(L2_SHARD)" ]; then \
		pattern="$$(GO="$(GO)" CGO_ENABLED=0 ./scripts/go_test_shard.sh "$(L2_SHARD)" -tags=multiproc ./test/l2/...)"; \
		run_flag="-run=$$pattern"; \
		echo "l2 shard $(L2_SHARD): $$pattern"; \
	fi; \
	log="$(TEST_LOG_DIR)/l2.log"; status_file="$$(mktemp)"; \
	trap 'rm -f "$$status_file"' EXIT; \
	echo 70 >"$$status_file"; \
	if [ -n "$(COVERPROFILE)" ]; then \
		cover_dir="$$(mktemp -d)"; \
		trap 'rm -rf "$$cover_dir"; rm -f "$$status_file"' EXIT; \
		{ if GOCOVERDIR="$$cover_dir" CGO_ENABLED=0 $(GO) test $(GO_TEST_FLAGS) $$run_flag -timeout=$(L2_TIMEOUT) -tags=multiproc ./test/l2/...; then echo 0 >"$$status_file"; else echo $$? >"$$status_file"; fi; } 2>&1 | tee "$$log"; \
		status="$$(cat "$$status_file")"; \
		if [ "$$status" != 0 ]; then echo "l2 failed (status $$status); full output: $$log"; exit "$$status"; fi; \
		$(GO) tool covdata textfmt -i="$$cover_dir" -o="$(COVERPROFILE)"; \
		test -s "$(COVERPROFILE)"; \
		$(GO) tool cover -func="$(COVERPROFILE)" | tail -1; \
	else \
		{ if CGO_ENABLED=0 $(GO) test $(GO_TEST_FLAGS) $$run_flag -timeout=$(L2_TIMEOUT) -tags=multiproc ./test/l2/...; then echo 0 >"$$status_file"; else echo $$? >"$$status_file"; fi; } 2>&1 | tee "$$log"; \
		status="$$(cat "$$status_file")"; \
		if [ "$$status" != 0 ]; then echo "l2 failed (status $$status); full output: $$log"; exit "$$status"; fi; \
	fi

## test-l3: container-level suite. Output and timeout are handled like
## test-l2's, for the same reasons: L3_TIMEOUT replaces `go test`'s 10m
## default, which a run has already crossed (FAIL test/l3 600.175s on a loaded
## runner, against 230-350s normally), and a package-level timeout panic names
## whichever case was in flight instead of what went wrong. The full output is
## kept in $(TEST_LOG_DIR)/l3.log so a failure can be read after the fact.
L3_TIMEOUT ?= 30m

.PHONY: test-l3
test-l3:
	@if command -v docker >/dev/null 2>&1 && docker info >/dev/null 2>&1; then \
		$(MAKE) fake-engine-image; \
	fi
	@set -eu; \
	mkdir -p $(TEST_LOG_DIR); \
	log="$(TEST_LOG_DIR)/l3.log"; status_file="$$(mktemp)"; \
	trap 'rm -f "$$status_file"' EXIT; \
	echo 70 >"$$status_file"; \
	if [ -n "$(COVERPROFILE)" ]; then \
		{ if CGO_ENABLED=0 $(GO) test $(GO_TEST_FLAGS) -timeout=$(L3_TIMEOUT) -coverprofile="$(COVERPROFILE)" -tags=docker ./internal/agent/runtime/docker ./internal/agent/engine \
			&& CGO_ENABLED=0 $(GO) test $(GO_TEST_FLAGS) -timeout=$(L3_TIMEOUT) -tags=docker ./test/l3/...; then echo 0 >"$$status_file"; else echo $$? >"$$status_file"; fi; } 2>&1 | tee "$$log"; \
		status="$$(cat "$$status_file")"; \
		if [ "$$status" != 0 ]; then echo "l3 failed (status $$status); full output: $$log"; exit "$$status"; fi; \
		test -s "$(COVERPROFILE)"; \
		$(GO) tool cover -func="$(COVERPROFILE)" | tail -1; \
	else \
		{ if CGO_ENABLED=0 $(GO) test $(GO_TEST_FLAGS) -timeout=$(L3_TIMEOUT) -tags=docker ./internal/agent/runtime/docker ./internal/agent/engine ./test/l3/...; then echo 0 >"$$status_file"; else echo $$? >"$$status_file"; fi; } 2>&1 | tee "$$log"; \
		status="$$(cat "$$status_file")"; \
		if [ "$$status" != 0 ]; then echo "l3 failed (status $$status); full output: $$log"; exit "$$status"; fi; \
	fi

## test-l3-kind: Kubernetes node support on a real kind cluster (needs docker and
## kind; skips without them unless GPUWORKS_TEST_STRICT=1). Builds the agent
## image the DaemonSet runs and the fake-engine image, which the test loads
## into the cluster.
KIND_AGENT_IMAGE ?= gpuworks/gpuworks-agent:l3kind

.PHONY: test-l3-kind
test-l3-kind:
	$(MAKE) agent-image AGENT_IMAGE=$(KIND_AGENT_IMAGE)
	$(MAKE) fake-engine-image
	GPUWORKS_KIND_AGENT_IMAGE=$(KIND_AGENT_IMAGE) CGO_ENABLED=0 $(GO) test -count=1 -v -tags=kind -timeout=30m ./test/l3kind/

.PHONY: test-l4-runpod-node-onboarding
test-l4-runpod-node-onboarding: cross-agents
	GPUWORKS_TEST_STRICT=1 CGO_ENABLED=0 $(GO) test $(GO_TEST_FLAGS) -v -tags=cloudgpu,runpod -run '^TestRunpodNodeOnboarding$$' -timeout=25m ./test/l4

.PHONY: test-l4-runpod-server-dial-node-onboarding
## Server-dialed agent on a pod, then a pod edit that renumbers its ports.
test-l4-runpod-server-dial-node-onboarding: cross-agents
	GPUWORKS_TEST_STRICT=1 CGO_ENABLED=0 $(GO) test $(GO_TEST_FLAGS) -v -tags=cloudgpu,runpod -run '^TestRunpodServerDialNodeOnboarding$$' -timeout=55m ./test/l4

.PHONY: test-l4-runpod-huggingface-public-download
## Explicit low-frequency public download acceptance. RunPod node onboarding
## above performs no registry pull and no model download.
test-l4-runpod-huggingface-public-download: cross-agents
	GPUWORKS_TEST_STRICT=1 CGO_ENABLED=0 $(GO) test $(GO_TEST_FLAGS) -v -count=1 -tags=cloudgpu,runpod -run '^TestRunpodHuggingFacePublicDownload$$' -timeout=25m ./test/l4

.PHONY: test-l4-runpod-vllm-sglang-lifecycle
test-l4-runpod-vllm-sglang-lifecycle: cross-agents
	GPUWORKS_TEST_STRICT=1 CGO_ENABLED=0 $(GO) test $(GO_TEST_FLAGS) -v -tags=cloudgpu,runpod -run '^TestRunpodVLLMSGLangLifecycle$$' -timeout=110m ./test/l4

.PHONY: test-l4-autodl-node-onboarding
test-l4-autodl-node-onboarding: cross-agents
	GPUWORKS_TEST_STRICT=1 CGO_ENABLED=0 $(GO) test $(GO_TEST_FLAGS) -v -tags=cloudgpu,autodl -run '^TestAutoDLNodeOnboarding$$' -timeout=85m ./test/l4

.PHONY: test-l4-vast-node-onboarding
test-l4-vast-node-onboarding: cross-agents
	GPUWORKS_TEST_STRICT=1 CGO_ENABLED=0 $(GO) test $(GO_TEST_FLAGS) -v -tags=cloudgpu,vast -run '^TestVastNodeOnboarding$$' -timeout=55m ./test/l4

.PHONY: test-l4-vast-server-dial-node-onboarding
test-l4-vast-server-dial-node-onboarding: cross-agents
	GPUWORKS_TEST_STRICT=1 CGO_ENABLED=0 $(GO) test $(GO_TEST_FLAGS) -v -tags=cloudgpu,vast -run '^TestVastServerDialNodeOnboarding$$' -timeout=55m ./test/l4

.PHONY: test-l4-gitee-ascend-partition-capability
test-l4-gitee-ascend-partition-capability: cross-agents
	GPUWORKS_TEST_STRICT=1 CGO_ENABLED=0 $(GO) test $(GO_TEST_FLAGS) -v -tags=cloudgpu,partition -run '^TestGiteeAscendPartitionCapability$$' -timeout=55m ./test/l4

.PHONY: test-l4-gitee-ascend-deploy-chat
## test-l4-gitee-ascend-deploy-chat: real server+agent on rented x86_64 and
## arm64 Gitee Ascend instances, driven through the GPUWorks platform API.
## Each runs a whole-GPU vLLM deployment (Huawei SWR image) serving a real
## small (~0.6B) ModelScope model and a /v1/chat/completions round trip.
test-l4-gitee-ascend-deploy-chat: cross-agents
	GPUWORKS_TEST_STRICT=1 CGO_ENABLED=0 $(GO) test $(GO_TEST_FLAGS) -v -tags=cloudgpu,partition -run '^TestGiteeAscend(FullServerAgentDeployChatEndToEnd|ARM64ContainerInContainerDeployChatEndToEnd)$$' -timeout=125m ./test/l4

.PHONY: test-l4-ppu-sglang-gateway-stability
test-l4-ppu-sglang-gateway-stability:
	GPUWORKS_TEST_STRICT=1 CGO_ENABLED=0 $(GO) test $(GO_TEST_FLAGS) -count=1 -v -tags=cloudgpu,partition -run '^TestPPUSGLangGatewayStability$$' -timeout=40m ./test/l4

.PHONY: test-l4-ascend-pool-partition-inference
## test-l4-ascend-pool-partition-inference: treat the configured self-owned Ascend hosts as one
## capability-selected fixture pool. The platform test uploads the checked
## current arm64 agent, starts it under an isolated /tmp state directory, then
## verifies two same-card partition replicas + real chat + quota denial and a
## whole-GPU deployment through the public platform APIs. Runtime is chosen per
## member: docker/nerdctl when it holds the image, else runveil (udocker driver)
## when the member has a prepared image cache at
## GPUWORKS_ASCEND_L4_RUNVEIL_CACHE (default /opt/gpuworks-l4-runveil-cache,
## udocker store layout: repos/ + layers/); its run directories go to
## <cache>-run so they stay on disk and hard-link the cache. The nerdctl checks
## independently select any pool member with nerdctl + the cached CANN image:
## one verifies raw device injection and one runs the embedded 2.5 MiB
## behavioral probe against a real 4 GiB vCANN slice (no serving image/model).
test-l4-ascend-pool-partition-inference: cross-agents
	GPUWORKS_TEST_STRICT=1 CGO_ENABLED=0 $(GO) test $(GO_TEST_FLAGS) -v -tags=cloudgpu,partition -run '^(TestAscendPoolPlatformPartitionInferenceAndWholeGPU|TestAscendNerdctl(DeviceInjection|PartitionCapabilityProbe)EndToEnd)$$' -timeout=28m ./test/l4

.PHONY: test-l4-ascend-primary
## test-l4-ascend-primary: real memory-quota + dual-container isolation check on
## the self-owned Ascend 910B4 machine that runs real Docker + Ascend-Docker-
## Runtime (not udocker — unaffected by the Gitee device-share restriction). The
## host is whatever GPUWORKS_ASCEND_PRIMARY_HOST names; do not read an address
## out of this comment (198.51.100.31 is a nerdctl-only pool member today).
test-l4-ascend-primary:
	GPUWORKS_TEST_STRICT=1 CGO_ENABLED=0 $(GO) test $(GO_TEST_FLAGS) -v -tags=cloudgpu,partition -run '^TestAscendSameCardDualContainerMemoryIsolation$$' -timeout=15m ./test/l4

.PHONY: test-l4-ascend-preflight
## test-l4-ascend-preflight: read-only SSH probe (npu-smi/docker, no containers)
## against the configured Ascend pool members (198.51.100.31 is one of them:
## measured aarch64 + nerdctl, no docker). Fast and non-destructive; runs before
## the deeper partition tests.
test-l4-ascend-preflight:
	GPUWORKS_TEST_STRICT=1 CGO_ENABLED=0 $(GO) test $(GO_TEST_FLAGS) -v -tags=cloudgpu,partition -run '^TestPartitionHardwarePreflight/Ascend' -timeout=5m ./test/l4

.PHONY: test-l4-ascend-nerdctl-devinject
## test-l4-ascend-nerdctl-devinject: real nerdctl device-injection and basic
## partition capability probe on whichever configured Ascend pool member has
## nerdctl. The test cross-compiles the arm64 helper from this revision, streams
## it over SSH, and the helper executes production engine/runtime code locally
## against the already embedded 2.5 MiB probe image; no serving image, model,
## framework, or inference engine is involved, and the GPU node needs no Go
## toolchain.
test-l4-ascend-nerdctl-devinject:
	GPUWORKS_TEST_STRICT=1 CGO_ENABLED=0 $(GO) test $(GO_TEST_FLAGS) -v -tags=cloudgpu,partition -run '^TestAscendNerdctl(DeviceInjection|PartitionCapabilityProbe)EndToEnd$$' -timeout=10m ./test/l4

.PHONY: test-l4-compshare
## The default CompShare matrix keeps one-VM logical-node and cross-VM Docker
## coverage. Cross-VM udocker remains available as an explicit low-frequency
## target but is not part of the default L4 acceptance run.
test-l4-compshare: test-l4-compshare-offline-contracts test-l4-compshare-single-vm-tp2-dgpu test-l4-compshare-provisioning-matrix test-l4-compshare-two-vm-docker-distributed test-l4-compshare-one-vm-two-logical-nodes

.PHONY: test-l4-compshare-modelscope-public-download
## Explicit low-frequency public download acceptance. Normal CompShare
## deployment cases require the template's preloaded model directory.
test-l4-compshare-modelscope-public-download: cross-agents
	GPUWORKS_TEST_STRICT=1 CGO_ENABLED=0 $(GO) test $(GO_TEST_FLAGS) -v -count=1 -tags=cloudgpu,compshare -run '^TestCompshareModelScopePublicDownload$$' -timeout=25m ./test/l4

.PHONY: prepare-compshare-golden-image
## Explicit, mutating maintenance operation. Creates a new custom image and
## validates it from a fresh VM; it does not change CI variables or delete the
## previous image.
prepare-compshare-golden-image:
	CGO_ENABLED=0 $(GO) run ./test/l4/cmd/prepare-compshare-golden

.PHONY: test-l4-compshare-offline-contracts
test-l4-compshare-offline-contracts: server-linux-amd64-prebuilt-ui
	GPUWORKS_TEST_STRICT=1 CGO_ENABLED=0 $(GO) test $(GO_TEST_FLAGS) -v -count=1 -tags=cloudgpu,compshare -run '^(TestCurrentCloudStartupAttempt|TestCloudStartupCommandDoesNotExposeProcessArguments|TestHosted.*|TestOneVMLogical.*|TestCompshareDockerInstall.*|TestCompshareGPUSelection.*|TestCompshareEngine.*)$$' -timeout=20m ./test/l4

.PHONY: test-l4-compshare-single-vm-tp2-dgpu
test-l4-compshare-single-vm-tp2-dgpu: cross-agents
	GPUWORKS_TEST_STRICT=1 CGO_ENABLED=0 $(GO) test $(GO_TEST_FLAGS) -v -count=1 -tags=cloudgpu,compshare -run '^TestCompshareSingleVMTensorParallelAndDGPU$$' -timeout=180m ./test/l4

.PHONY: test-l4-compshare-provisioning-matrix
test-l4-compshare-provisioning-matrix: cross-agents
	GPUWORKS_TEST_STRICT=1 CGO_ENABLED=0 $(GO) test $(GO_TEST_FLAGS) -v -count=1 -tags=cloudgpu,compshare -run '^(TestCompshareNodeOnboarding|TestCompshareDirectServerDialOnboarding|TestCompshareCapacityShapes|TestCompshareTwoGPUProvisioning)$$' -timeout=180m ./test/l4

.PHONY: test-l4-compshare-server-dial-node-onboarding-container
## Server-dialed agent on a CompShare container instance, reached through the
## TCP port the driver opens and forwards. Needs COMPSHARE_CONTAINER_IMAGE_ID;
## the VM counterpart is TestCompshareDirectServerDialOnboarding in the matrix.
test-l4-compshare-server-dial-node-onboarding-container: cross-agents
	GPUWORKS_TEST_STRICT=1 CGO_ENABLED=0 $(GO) test $(GO_TEST_FLAGS) -v -count=1 -tags=cloudgpu,compshare -run '^TestCompshareServerDialNodeOnboardingContainer$$' -timeout=85m ./test/l4

.PHONY: test-l4-compshare-two-vm-udocker-distributed
test-l4-compshare-two-vm-udocker-distributed: server-linux-amd64-prebuilt-ui
	GPUWORKS_TEST_STRICT=1 CGO_ENABLED=0 $(GO) test $(GO_TEST_FLAGS) -v -count=1 -tags=cloudgpu,compshare -run '^TestCompshareTwoVMUdockerDistributedInference$$' -timeout=180m ./test/l4

.PHONY: test-l4-compshare-two-vm-docker-distributed
test-l4-compshare-two-vm-docker-distributed: server-linux-amd64-prebuilt-ui
	GPUWORKS_TEST_STRICT=1 CGO_ENABLED=0 $(GO) test $(GO_TEST_FLAGS) -v -count=1 -tags=cloudgpu,compshare -run '^TestCompshareTwoVMDockerDistributedInference$$' -timeout=180m ./test/l4

.PHONY: test-l4-compshare-pd-deploy-chat-two-vm-docker
test-l4-compshare-pd-deploy-chat-two-vm-docker: server-linux-amd64-prebuilt-ui
	GPUWORKS_TEST_STRICT=1 CGO_ENABLED=0 $(GO) test $(GO_TEST_FLAGS) -v -count=1 -tags=cloudgpu,compshare -run '^TestCompsharePDDeployChatTwoVMDocker$$' -timeout=180m ./test/l4

.PHONY: test-l4-compshare-one-vm-two-logical-nodes
test-l4-compshare-one-vm-two-logical-nodes: server-linux-amd64-prebuilt-ui
	GPUWORKS_TEST_STRICT=1 CGO_ENABLED=0 $(GO) test $(GO_TEST_FLAGS) -v -count=1 -tags=cloudgpu,compshare -run '^TestCompshareOneVMTwoLogicalNodeDistributedInference$$' -timeout=180m ./test/l4

.PHONY: test-l4-aliyun
## Aliyun ECS L4. VMs are rented spot-first, falling back to on-demand when
## spot has no stock or is refused, from a golden custom image (NVIDIA driver,
## Docker 29, NVIDIA toolkit, vLLM runner image, test model) that
## prepare-aliyun-golden-image builds and copies to every region; cases
## install and download nothing. See docs/dev/testing.md.
## Cross-VM udocker and public download stay explicit low-frequency targets, as for CompShare.
test-l4-aliyun: test-l4-aliyun-provisioning-matrix test-l4-aliyun-single-vm-tp2-dgpu test-l4-aliyun-one-vm-two-logical-nodes test-l4-aliyun-two-vm-docker-distributed test-l4-aliyun-pd-deploy-chat-two-vm-docker

.PHONY: prepare-aliyun-golden-image
## Explicit, mutating maintenance operation: builds a new golden image in the
## cheapest region with stock, copies it to the other regions and validates it
## from a fresh VM. It prints the ALIYUN_IMAGE_<REGION> lines, changes no CI
## variable and deletes no older image.
prepare-aliyun-golden-image:
	CGO_ENABLED=0 $(GO) run ./test/l4/cmd/prepare-aliyun-golden

.PHONY: test-l4-aliyun-provisioning-matrix
test-l4-aliyun-provisioning-matrix: cross-agents
	GPUWORKS_TEST_STRICT=1 CGO_ENABLED=0 $(GO) test $(GO_TEST_FLAGS) -v -count=1 -tags=cloudgpu,aliyun -run '^(TestAliyunNodeOnboarding|TestAliyunDirectServerDialOnboarding|TestAliyunCapacityShapes|TestAliyunTwoGPUProvisioning)$$' -timeout=180m ./test/l4

.PHONY: test-l4-aliyun-modelscope-public-download
## Explicit low-frequency public download acceptance (the only Aliyun case that
## downloads a model; the others use the golden image's preloaded one).
test-l4-aliyun-modelscope-public-download: cross-agents
	GPUWORKS_TEST_STRICT=1 CGO_ENABLED=0 $(GO) test $(GO_TEST_FLAGS) -v -count=1 -tags=cloudgpu,aliyun -run '^TestAliyunModelScopePublicDownload$$' -timeout=40m ./test/l4

.PHONY: test-l4-aliyun-single-vm-tp2-dgpu
test-l4-aliyun-single-vm-tp2-dgpu: cross-agents
	GPUWORKS_TEST_STRICT=1 CGO_ENABLED=0 $(GO) test $(GO_TEST_FLAGS) -v -count=1 -tags=cloudgpu,aliyun -run '^TestAliyunSingleVMTensorParallelAndDGPU$$' -timeout=180m ./test/l4

.PHONY: test-l4-aliyun-one-vm-two-logical-nodes
test-l4-aliyun-one-vm-two-logical-nodes: server-linux-amd64-prebuilt-ui
	GPUWORKS_TEST_STRICT=1 CGO_ENABLED=0 $(GO) test $(GO_TEST_FLAGS) -v -count=1 -tags=cloudgpu,aliyun -run '^TestAliyunOneVMTwoLogicalNodeDistributedInference$$' -timeout=180m ./test/l4

.PHONY: test-l4-aliyun-two-vm-docker-distributed
test-l4-aliyun-two-vm-docker-distributed: server-linux-amd64-prebuilt-ui
	GPUWORKS_TEST_STRICT=1 CGO_ENABLED=0 $(GO) test $(GO_TEST_FLAGS) -v -count=1 -tags=cloudgpu,aliyun -run '^TestAliyunTwoVMDockerDistributedInference$$' -timeout=180m ./test/l4

.PHONY: test-l4-aliyun-two-vm-udocker-distributed
test-l4-aliyun-two-vm-udocker-distributed: server-linux-amd64-prebuilt-ui
	GPUWORKS_TEST_STRICT=1 CGO_ENABLED=0 $(GO) test $(GO_TEST_FLAGS) -v -count=1 -tags=cloudgpu,aliyun -run '^TestAliyunTwoVMUdockerDistributedInference$$' -timeout=180m ./test/l4

.PHONY: test-l4-aliyun-pd-deploy-chat-two-vm-docker
test-l4-aliyun-pd-deploy-chat-two-vm-docker: server-linux-amd64-prebuilt-ui
	GPUWORKS_TEST_STRICT=1 CGO_ENABLED=0 $(GO) test $(GO_TEST_FLAGS) -v -count=1 -tags=cloudgpu,aliyun -run '^TestAliyunPDDeployChatTwoVMDocker$$' -timeout=180m ./test/l4

.PHONY: test-l4-aliyun-sglang-pd-multinode-prefill-two-vm-docker
## Explicit manual case (not in test-l4-aliyun, no CI job yet): a SGLang PD
## deployment whose prefill instance spans both VMs. Each VM pulls the SGLang
## runner once; the golden image carries only the vLLM one.
test-l4-aliyun-sglang-pd-multinode-prefill-two-vm-docker: server-linux-amd64-prebuilt-ui
	GPUWORKS_TEST_STRICT=1 CGO_ENABLED=0 $(GO) test $(GO_TEST_FLAGS) -v -count=1 -tags=cloudgpu,aliyun -run '^TestAliyunSGLangPDMultiNodePrefillTwoVMDocker$$' -timeout=180m ./test/l4

.PHONY: test-l4-runpod
test-l4-runpod: test-l4-runpod-vllm-sglang-lifecycle

.PHONY: test-l4
test-l4: test-l4-runpod test-l4-autodl-node-onboarding test-l4-vast-node-onboarding test-l4-compshare test-l4-aliyun

.PHONY: test-l4-cleanup-leaked
test-l4-cleanup-leaked:
	@test -n "$(GPUWORKS_CLOUD_TEST_RUN_ID)" || (echo "GPUWORKS_CLOUD_TEST_RUN_ID is required for scoped cleanup" >&2; exit 2)
	CGO_ENABLED=0 $(GO) run ./test/l4/cmd/cleanup-leaked --run-id "$(GPUWORKS_CLOUD_TEST_RUN_ID)"

.PHONY: check-libvgpu
## check-libvgpu: enforce the glibc baseline of the embedded HAMi asset
check-libvgpu:
	./scripts/check_libvgpu_compat.sh

.PHONY: check-perftest
## check-perftest: enforce the library and symbol baseline of the embedded ib_write_bw
check-perftest:
	./scripts/check_perftest_compat.sh

.PHONY: vet
## vet: static analysis
vet:
	CGO_ENABLED=0 $(GO) vet ./...
	cd pkg/extapi && CGO_ENABLED=0 $(GO) vet ./...
	cd pkg/pluginsdk && CGO_ENABLED=0 $(GO) vet ./...

.PHONY: lint
## lint: run the complete pinned Go lint rule set, including tests.
lint: $(LINT_BIN)
	@test -f internal/server/webui/dist/index.html || $(MAKE) ui
	CGO_ENABLED=0 $(LINT_BIN) run --concurrency=4 --timeout=12m --max-issues-per-linter=0 --max-same-issues=0

$(LINT_BIN):
	mkdir -p $(dir $(LINT_BIN))
	GOMAXPROCS=4 GOTOOLCHAIN=$(LINT_GO_VERSION) GOBIN=$(dir $(LINT_BIN)) $(GO) install -p=4 github.com/golangci/golangci-lint/cmd/golangci-lint@$(GOLANGCI_LINT_VERSION)

.PHONY: tidy
## tidy: sync go.mod/go.sum
tidy:
	$(GO) mod tidy

.PHONY: run-server
## run-server: build and run the server locally
run-server: server
	$(BIN)/gpuworks serve

.PHONY: demo
## demo: run the scripted M0 acceptance demo
demo: build cross
	./scripts/demo_m0.sh

.PHONY: clean
## clean: remove build artifacts
clean:
	rm -rf $(BIN) internal/server/webui/dist/assets data

.PHONY: test-e2e-log-filter-export
## test-e2e-log-filter-export: real local server WARN collection, browser filters and exact CSV bytes
test-e2e-log-filter-export: ensure-bins
	NO_PROXY=127.0.0.1,localhost no_proxy=127.0.0.1,localhost python3 test/e2e/log_filter_export.py --locale en
	NO_PROXY=127.0.0.1,localhost no_proxy=127.0.0.1,localhost python3 test/e2e/log_filter_export.py --locale zh

include $(wildcard test/e2e/*.mk)

.PHONY: test-l4-storage-model-cache-copy-nfs
# Explicit read-only fixture, at most 16 MiB. No production fault injection.
test-l4-storage-model-cache-copy-nfs:
	GPUWORKS_TEST_STRICT=1 CGO_ENABLED=0 $(GO) test $(GO_TEST_FLAGS) -count=1 -v -tags=l4 -timeout=5m ./internal/agent/app -run '^TestL4StorageModelCacheCopyNFS$$'

.PHONY: test-l4-storage-model-cache-recovery-nfs
# Explicit disposable Ganesha image; requires root + a private mount namespace.
test-l4-storage-model-cache-recovery-nfs:
	GPUWORKS_TEST_STRICT=1 CGO_ENABLED=0 $(GO) test $(GO_TEST_FLAGS) -count=1 -v -tags=l4 -timeout=20m ./test/l4/nfsfault -run '^TestL4StorageModelCacheRecoveryNFS$$'
