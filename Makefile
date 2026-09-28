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

.PHONY: all build clean db-down db-psql db-reset db-up demo demo-idempotency demo-post demo-serve dist fmt image lint negative-controls run test vet

all: build vet lint test

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

## run: start the database, build, and serve the ledger on $(ADDR)
run: db-up build
	$(BIN_DIR)/$(BINARY) serve -addr $(ADDR)

## demo: reset the database and show the ledger working end to end over HTTP
demo:
	@$(MAKE) --no-print-directory db-reset
	@$(MAKE) --no-print-directory -s build
	@tools/demo.sh $(BIN_DIR)/$(BINARY) $(DEMO_ADDR); status=$$?; \
		$(MAKE) --no-print-directory db-reset; exit $$status
## demo-idempotency: one request sent twice under one key, then sixteen at once
demo-idempotency:
	@$(MAKE) --no-print-directory db-reset
	@$(MAKE) --no-print-directory -s build
	@tools/idempotency-demo.sh $(BIN_DIR)/$(BINARY) $(DEMO_ADDR); status=$$?; \
		if [ $$status -eq 0 ]; then \
			echo; \
			echo '$$ go test -race -count=1 -run TestManyRequestsWithOneKeyAtOnceWriteOnce -v ./internal/ledgerhttp/'; \
			go test -race -count=1 -run TestManyRequestsWithOneKeyAtOnceWriteOnce -v ./internal/ledgerhttp/ || status=$$?; \
		fi; \
		$(MAKE) --no-print-directory db-reset; exit $$status

## demo-post: a transaction taken and one refused, with the balance either side
demo-post:
	@$(MAKE) --no-print-directory db-reset
	@$(MAKE) --no-print-directory -s build
	@tools/post-demo.sh $(BIN_DIR)/$(BINARY) $(DEMO_ADDR); status=$$?; \
		$(MAKE) --no-print-directory db-reset; exit $$status

## demo-serve: the binary serving on a socket, then a clean exit on SIGINT
demo-serve:
	@$(MAKE) --no-print-directory db-reset
	@$(MAKE) --no-print-directory -s build
	@tools/serve-demo.sh $(BIN_DIR)/$(BINARY) $(DEMO_ADDR); status=$$?; \
		$(MAKE) --no-print-directory db-reset; exit $$status
## negative-controls: apply every declared mutation and check each test turns red
negative-controls:
	@$(MAKE) --no-print-directory db-reset
	@go run ./tools/controls; status=$$?; \
		$(MAKE) --no-print-directory db-reset; exit $$status
## db-up: start the test database and block until it answers
db-up:
	$(COMPOSE) up --detach --wait

## db-reset: drop the database and start it again, so the schema is applied fresh
db-reset:
	@$(COMPOSE) down --volumes --remove-orphans >/dev/null 2>&1
	@$(COMPOSE) up --detach --wait >/dev/null 2>&1

## db-down: stop the test database and delete everything in it
db-down:
	$(COMPOSE) down --volumes --remove-orphans

## db-psql: open a shell on the test database
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
	@out="$$(gofmt -l .)"; \
	if [ -n "$$out" ]; then echo "gofmt needed:"; echo "$$out"; exit 1; fi

## clean: remove build output and the test database
clean: db-down
	rm -rf $(BIN_DIR) $(DIST_DIR)
