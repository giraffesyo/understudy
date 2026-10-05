export CGO_ENABLED := 0

GO       ?= go
BIN      := bin
DIST     := dist
AGENTS   := internal/embedded/agents
LDFLAGS  := -s -w

# VERSION is stamped into `understudy version`: `git describe` output, or
# make VERSION=x. Release artifacts are built by GoReleaser
# (.goreleaser.yaml), which stamps the release tag (v1.2.3) the same way.
VERSION  ?= $(shell git describe --tags --match 'v[0-9]*' --always --dirty 2>/dev/null || echo 0.1.0-dev)
VERSION_LDFLAGS := -X github.com/giraffesyo/understudy/internal/cli.version=$(VERSION)

# Control-binary release targets (the embedded agents are always linux);
# keep in step with `builds` in .goreleaser.yaml.
RELEASE_PLATFORMS := linux/amd64 linux/arm64 darwin/amd64 darwin/arm64

.PHONY: build agents test test-e2e test-golden depcheck cross clean

build: agents
	$(GO) build -trimpath -ldflags="$(VERSION_LDFLAGS)" -o $(BIN)/understudy ./cmd/understudy
	ln -sf understudy $(BIN)/ansible-playbook
	ln -sf understudy $(BIN)/ansible

agents: depcheck
	CGO_ENABLED=0 GOOS=linux GOARCH=amd64 $(GO) build -trimpath -ldflags="$(LDFLAGS)" -o $(AGENTS)/agent-linux-amd64 ./cmd/agent
	CGO_ENABLED=0 GOOS=linux GOARCH=arm64 $(GO) build -trimpath -ldflags="$(LDFLAGS)" -o $(AGENTS)/agent-linux-arm64 ./cmd/agent
	gzip -9 -n -f $(AGENTS)/agent-linux-amd64 $(AGENTS)/agent-linux-arm64

depcheck:
	$(GO) run ./tools/depcheck ./cmd/agent

test:
	$(GO) test ./...

test-e2e:
	$(GO) test -tags e2e -timeout 45m ./test/e2e/...

# Differential tests vs. real ansible-playbook (must be on PATH, or set
# UNDERSTUDY_ANSIBLE_PLAYBOOK). Requires `make build` first.
test-golden: build
	$(GO) test -tags golden -timeout 60m ./test/e2e/...

# Cross-compile the control binary for every release platform (build check
# only; nothing is kept). Release artifacts come from GoReleaser: see
# "Releases" in the README.
cross: agents
	@set -e; for p in $(RELEASE_PLATFORMS); do \
		echo "go build $$p"; \
		CGO_ENABLED=0 GOOS=$${p%/*} GOARCH=$${p#*/} $(GO) build -trimpath -o /dev/null ./cmd/understudy; \
	done

# dist/ is GoReleaser's output directory.
clean:
	rm -rf $(BIN) $(DIST) $(AGENTS)/agent-*
