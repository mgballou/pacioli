# pacioli

BINARY      := pacioli
CMD         := ./cmd/pacioli
BIN_DIR     := bin
VERSION     ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)
LDFLAGS     := -X main.version=$(VERSION)
STATICCHECK := honnef.co/go/tools/cmd/staticcheck@2025.1.1
COMPOSE     := docker compose -f compose.test.yaml
DIST_DIR    := dist
TARGETS     := linux/amd64 linux/arm64 darwin/amd64 darwin/arm64
IMAGE       ?= pacioli-ledger
REVISION    ?= $(shell git rev-parse HEAD 2>/dev/null || echo unknown)

# Loopback only, so `make run` cannot expose the lab database to the network.
ADDR        ?= 127.0.0.1:8080

# A different port for the demo, so it cannot collide with a running `make run`.
DEMO_ADDR ?= 127.0.0.1:58080

.PHONY: all build clean compose-smoke db-down db-psql db-reset db-up demo demo-idempotency demo-post demo-serve dist fmt fuzz help image lint negative-controls run test vet

## all: check formatting, build, vet, lint, and run every race test (default)
all: fmt build vet lint test fuzz

## help: list every target and its purpose
help:
	@printf '%s\n' 'Usage: make [target] (default: all)' 'Overrides: ADDR=$(ADDR) DEMO_ADDR=$(DEMO_ADDR)' ''
	@awk '/^## / { sub(/^## /, ""); split($$0, parts, ": "); printf "  %-20s %s\n", parts[1], substr($$0, length(parts[1]) + 3) }' $(MAKEFILE_LIST)

## fuzz: fuzz each request boundary and amount JSON round-trip for ten seconds
fuzz:
	go test -run=^$$ -fuzz=FuzzAccountBody -fuzztime=10s ./internal/ledgerhttp
	go test -run=^$$ -fuzz=FuzzTransactionBody -fuzztime=10s ./internal/ledgerhttp
	go test -run=^$$ -fuzz=FuzzMinorJSONRoundTrip -fuzztime=10s ./internal/ledger

## build: compile the binary into bin/, stamped with the git version
build:
	go build -ldflags '$(LDFLAGS)' -o $(BIN_DIR)/$(BINARY) $(CMD)

## dist: cross-compile a stamped binary per release target into dist/, with checksums
dist:
	rm -rf $(DIST_DIR)
	@for t in $(TARGETS); do \
		os=$${t%/*}; arch=$${t#*/}; out=$(DIST_DIR)/$(BINARY)-$(VERSION)-$$os-$$arch; \
		echo "$$out"; \
		CGO_ENABLED=0 GOOS=$$os GOARCH=$$arch go build -trimpath -ldflags '$(LDFLAGS)' -o $$out $(CMD) || exit 1; \
	done
	cd $(DIST_DIR) && shasum -a 256 $(BINARY)-* > SHA256SUMS

## image: build the container image as $(IMAGE):$(VERSION), labeled with the version and commit
image:
	docker build \
		--build-arg VERSION=$(VERSION) \
		--label org.opencontainers.image.version=$(VERSION) \
		--label org.opencontainers.image.revision=$(REVISION) \
		--label org.opencontainers.image.source=https://github.com/mgballou/pacioli \
		--label org.opencontainers.image.licenses=MIT \
		-t $(IMAGE):$(VERSION) .

## test: run every test with the race detector enabled, against a live database
test: db-up
	go test -race ./...

## run: start the database, build, and serve on ADDR; Ctrl-C stops it
run: db-up build
	$(BIN_DIR)/$(BINARY) serve -addr $(ADDR)

## demo: reset the database and show the ledger working end to end over HTTP
demo:
	@$(MAKE) --no-print-directory db-reset
	@$(MAKE) --no-print-directory -s build
	@tools/demo.sh $(BIN_DIR)/$(BINARY) $(DEMO_ADDR); status=$$?; \
		$(MAKE) --no-print-directory db-reset || exit $$?; exit $$status
## demo-idempotency: reset the database, send one key twice, then sixteen times at once
demo-idempotency:
	@$(MAKE) --no-print-directory db-reset
	@$(MAKE) --no-print-directory -s build
	@tools/idempotency-demo.sh $(BIN_DIR)/$(BINARY) $(DEMO_ADDR); status=$$?; \
		if [ $$status -eq 0 ]; then \
			echo; \
			echo '$$ go test -race -count=1 -run TestManyRequestsWithOneKeyAtOnceWriteOnce -v ./internal/ledgerhttp/'; \
			go test -race -count=1 -run TestManyRequestsWithOneKeyAtOnceWriteOnce -v ./internal/ledgerhttp/ || status=$$?; \
		fi; \
		$(MAKE) --no-print-directory db-reset || exit $$?; exit $$status

## demo-post: reset the database, show a transaction taken and one refused
demo-post:
	@$(MAKE) --no-print-directory db-reset
	@$(MAKE) --no-print-directory -s build
	@tools/post-demo.sh $(BIN_DIR)/$(BINARY) $(DEMO_ADDR); status=$$?; \
		$(MAKE) --no-print-directory db-reset || exit $$?; exit $$status

## demo-serve: reset the database, serve on a socket, then exit on SIGINT
demo-serve:
	@$(MAKE) --no-print-directory db-reset
	@$(MAKE) --no-print-directory -s build
	@tools/serve-demo.sh $(BIN_DIR)/$(BINARY) $(DEMO_ADDR); status=$$?; \
		$(MAKE) --no-print-directory db-reset || exit $$?; exit $$status
## compose-smoke: build a separate stack on port 8080 and check its empty trial balance
compose-smoke:
	@tools/compose-smoke.sh
## negative-controls: reset the database and check every mutation turns red; needs a clean Git tree
negative-controls:
	@$(MAKE) --no-print-directory db-reset
	@go run ./tools/controls; status=$$?; \
		$(MAKE) --no-print-directory db-reset || exit $$?; exit $$status
## db-up: start the test database and block until it answers
db-up:
	$(COMPOSE) up --detach --wait

## db-reset: delete the test database and start it again empty
db-reset:
	@$(COMPOSE) down --volumes --remove-orphans >/dev/null 2>&1
	@$(COMPOSE) up --detach --wait >/dev/null 2>&1

## db-down: stop the test database and delete everything in it
db-down:
	$(COMPOSE) down --volumes --remove-orphans

## db-psql: open a shell on the test database; run db-up first
db-psql:
	$(COMPOSE) exec postgres psql -U ledger -d ledger_test

## vet: run the toolchain's own correctness checks
vet:
	go vet ./...

## lint: run staticcheck at the version pinned above
lint:
	go run $(STATICCHECK) ./...

## fmt: report any file gofmt would rewrite, and fail if there are any
fmt:
	@out="$$(gofmt -l .)" || exit $$?; \
	if [ -n "$$out" ]; then echo "gofmt needed:"; echo "$$out"; exit 1; fi

## clean: remove build output and the test database
clean: db-down
	rm -rf $(BIN_DIR) $(DIST_DIR)
