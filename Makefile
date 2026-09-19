# pacioli

BINARY      := pacioli
CMD         := ./cmd/pacioli
BIN_DIR     := bin
VERSION     ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)
LDFLAGS     := -X main.version=$(VERSION)
STATICCHECK := honnef.co/go/tools/cmd/staticcheck@2025.1.1
COMPOSE     := docker compose -f compose.test.yaml

# Loopback only, so `make run` cannot expose the lab database to the network.
ADDR        ?= 127.0.0.1:8080

# A different port for the demo, so it cannot collide with a running `make run`.
DEMO_ADDR ?= 127.0.0.1:58080

.PHONY: all build clean db-down db-psql db-reset db-up demo-post demo-serve fmt lint negative-controls run test vet

all: build vet lint test

## build: compile the binary into bin/, stamped with the git version
build:
	go build -ldflags '$(LDFLAGS)' -o $(BIN_DIR)/$(BINARY) $(CMD)

## test: run every test with the race detector enabled, against a live database
test: db-up
	go test -race ./...

## run: start the database, build, and serve the ledger on $(ADDR)
run: db-up build
	$(BIN_DIR)/$(BINARY) serve -addr $(ADDR)

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
	rm -rf $(BIN_DIR)
