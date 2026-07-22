GO       ?= go
BIN      := bin
AGENTS   := internal/embedded/agents
LDFLAGS  := -s -w

.PHONY: build agents test test-e2e test-golden depcheck clean

build: agents
	$(GO) build -trimpath -o $(BIN)/understudy ./cmd/understudy
	ln -sf understudy $(BIN)/ansible-playbook
	ln -sf understudy $(BIN)/ansible

agents: depcheck
	CGO_ENABLED=0 GOOS=linux GOARCH=amd64 $(GO) build -trimpath -ldflags="$(LDFLAGS)" -o $(AGENTS)/agent-linux-amd64 ./cmd/agent
	CGO_ENABLED=0 GOOS=linux GOARCH=arm64 $(GO) build -trimpath -ldflags="$(LDFLAGS)" -o $(AGENTS)/agent-linux-arm64 ./cmd/agent
	gzip -9 -f $(AGENTS)/agent-linux-amd64 $(AGENTS)/agent-linux-arm64

depcheck:
	$(GO) run ./tools/depcheck ./cmd/agent

test:
	$(GO) test ./...

test-e2e:
	$(GO) test -tags e2e ./test/e2e/...

# Differential tests vs. real ansible-playbook (must be on PATH, or set
# UNDERSTUDY_ANSIBLE_PLAYBOOK). Requires `make build` first.
test-golden: build
	$(GO) test -tags golden ./test/e2e/...

clean:
	rm -rf $(BIN) $(AGENTS)/agent-*
