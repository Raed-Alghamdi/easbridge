# EASBridge build helpers. Every target loads the project-local Go toolchain.
SHELL := /bin/bash
ENV   := export EASBRIDGE_ENV_QUIET=1; source $(CURDIR)/env.sh
VERSION ?= 0.3.0
# IMAGE changes where `easbridge init` says the image lives (internal/version.Image) for this build.
IMAGE ?=
LDFLAGS := -s -w -X easbridge/internal/version.Version=$(VERSION) $(if $(IMAGE),-X easbridge/internal/version.Image=$(IMAGE))

.PHONY: all go-version build build-devfake vet test test-devfake test-vendored check check-prod compat e2e vulncheck clean

all: check

# Which Go the targets use (the project-local toolchain in .toolchain/go, else the Go on your PATH).
go-version:
	@EASBRIDGE_ENV_QUIET= bash -c 'source $(CURDIR)/env.sh'

build:
	@$(ENV) && mkdir -p bin && CGO_ENABLED=0 go build -trimpath -ldflags '$(LDFLAGS)' -o bin/easbridge ./cmd/easbridge

build-devfake:
	@$(ENV) && mkdir -p bin && CGO_ENABLED=0 go build -tags devfake -trimpath -o bin/easbridge-devfake ./cmd/easbridge

vet:
	@$(ENV) && go vet ./... && go vet -tags devfake ./... && (cd third_party/go-activesync && go vet ./...)

test:
	@$(ENV) && go test ./...

test-devfake:
	@$(ENV) && go test -tags devfake ./...

# go-activesync lives in its own module (replace directive), so ./... skips it.
test-vendored:
	@$(ENV) && (cd third_party/go-activesync && go test ./...)

# The production binary must not contain the fake upstream (devfake build tag).
check-prod: build
	@for pat in fakeupstream 'FAKE UPSTREAM' EASBRIDGE_FAKE_PASSWORD; do \
	  if grep -a -q "$$pat" bin/easbridge; then echo "bin/easbridge contains $$pat"; exit 1; fi; \
	done; echo "production binary is free of the fake upstream"

check: go-version vet test test-devfake test-vendored check-prod

# Known vulnerabilities in the dependencies (pinned govulncheck, project-local
# toolchain and caches; needs the network for the vulnerability database).
vulncheck:
	@./scripts/govulncheck.sh

# The install path in Docker (build a devfake image, easbridge init, docker compose up, a Node backend
# that waits for the bridge and connects with verification on). Skips cleanly when Docker is not usable.
e2e:
	@./e2e/run.sh

# imapflow/nodemailer compatibility harness (needs node and npm; talks only to a
# local devfake bridge, never to a real server).
compat:
	@./compat/run.sh

clean:
	rm -rf bin
