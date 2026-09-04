# Contributing

## Getting set up

Needs Go 1.26 and a container runtime, Docker or OrbStack. Everything else is the
Makefile.

```
make all                 # build, vet, staticcheck, race tests
make test                # race tests only; brings the database up itself
make run                 # database up, build, serve on $(ADDR) — 127.0.0.1:8080
make demo                # the ledger working, in seven steps over HTTP
make demo-post           # a transaction taken and one refused, balances either side
make demo-idempotency    # one key sent twice, then sixteen of it at once
make demo-serve          # the binary on a socket, four exchanges, a clean exit
make db-up               # start the test database and block until it answers
make db-reset            # drop the test database and start it again
make db-down             # stop it and delete everything in it
make fmt                 # fail if gofmt would rewrite any file
make negative-controls   # every declared mutation, green before and red after
```

The test database is Postgres 18 on `127.0.0.1:55432`, not 5432, so it cannot
reach a server already running on the machine. Its data directory is a tmpfs, so
it starts empty every time.

`make all` has to pass before a change is finished. `docs/DESIGN.md` gives the
reasoning behind the rules below; read it before arguing with one.

## Conventions

- **Reach for the standard library first.** A new dependency needs an argument in
  the change that adds it. `pgx` is the only one, and only its `database/sql`
  driver registration is imported. `staticcheck` is fetched by `go run` at a
  pinned version, so it never enters `go.mod`.
- **Put constraints in Postgres, never in Go.** `internal/schema` embeds one
  hand-written SQL file and runs it verbatim. `ledger.Post` has no validation
  branch: it writes, asks the server, and translates the refusal into
  `ErrUnbalanced`, `ErrUnknownAccount`, `ErrCurrencyMismatch` or `ErrRejected`,
  each wrapping the server's own `*pgconn.PgError`.
- **Keep one schema file.** There is no migration framework and no ORM. A second
  schema file is the moment that argument changes.
- **Use signed minor units.** Debit positive, credit negative. Never a float,
  never two columns.
- **Derive balances; never store one.** Postgres computes them in one statement
  on the caller's own `*sql.Tx`. A trial balance is per currency and never
  across.
- **Refuse a closed set; answer an open one with nothing.** The five
  `account_kind` values are closed, so `?kind=liabilty` is a 400. Currencies are
  open, so `?currency=ZWL` is a 200 over `[]`. The query parameters an endpoint
  defines are closed too, so `?curency=GBP` is a 400 naming the parameter.
- **Read the closed set from Postgres.** `ledger.checkKind` asks
  `enum_range(NULL::account_kind)` rather than holding the five in Go, so a sixth
  kind needs only a schema edit.
- **Keep the binary a wiring layer.** `cmd/pacioli serve` mounts
  `ledgerhttp.Handler` and adds no route of its own. It is a listener, a pool, a
  signal and a drain. Draining is not optional: a client cannot tell a cut
  connection from a network fault, so it retries.
- **Require an `Idempotency-Key` on `POST /v1/transactions`.** `ledger.Once`
  reserves the key with one `INSERT` before the entry is written. `Once` takes
  the write as a function so that reserving and writing cannot come apart. Do not
  replace this with a read followed by a write. Decision 11 in `docs/DESIGN.md`
  says why.
- **Return 400 when the request could not be read and 422 when the ledger refused
  it.** Nothing in Go decides either one.
- **Own the wire types.** `internal/ledgerhttp` converts `ledger.Balance` into a
  struct of its own rather than hanging JSON tags on the domain, so renaming a
  domain field breaks a compile instead of a published API. Routing is
  `http.ServeMux` with the method in the pattern. There is no router dependency.
- **Give a handler a `Store`, never a pool.** `ledgerhttp.Pool` is the one a
  server runs on. A test supplies its own and hands the handler the transaction
  the test is already inside. Do not simplify `Store` to a `*sql.DB`.
- **Test against a real database, never a mock.** `testdb.Tx(t)` hands a test a
  transaction that is rolled back when it ends, so order cannot matter. Tests are
  external — `package ledger_test` — and named as sentences, such as
  `TestARefusedEntryLeavesNothingBehindAndTheCallerCarriesOn`.
- **Keep comments short.** A package comment says what the package does in one to
  three lines. An exported identifier gets one line in standard Go form, two if
  the contract needs it. An inline comment goes only where the code would
  surprise a competent reader. Reasoning that runs to a paragraph belongs in
  `docs/DESIGN.md`.

## Adding a negative control

A control declares a mutation and names the test that should catch it.
`make negative-controls` applies each one, runs its test, and insists the test is
green before the mutation and red after.

A control that mutates the schema must not be the last one in the run. The
container keeps the mutated schema until something resets it, and `schema.Apply`
is a no-op against a warm container, so the next `go test` would quietly ask a
ledger with no rule. Control 080 mutates the account code's `UNIQUE` and 081
begins with `make db-reset &&` for that reason, the same way 075 covers 074. The
last control has to leave the shared container whole.

## Traps

- **A deferred constraint is never asked in a test that rolls back.** The balance
  triggers are `INITIALLY DEFERRED` and tests never commit, so a test that only
  inserts bad rows passes. Call `SET CONSTRAINTS ALL IMMEDIATE` — `mustSettle` in
  `internal/schema/schema_test.go` — or the test cannot fail.
- **`SET CONSTRAINTS ALL IMMEDIATE` reaches the idempotency trigger too.** A test
  that settles and then keeps writing has to `SET CONSTRAINTS ALL DEFERRED`
  again, or the reservation — a row that legitimately holds no result yet — is
  refused the moment it goes in. `ledger.Post` and `ledger.Once` both defer their
  own before they write. `mustDefer` is the hand-written version.
- **Change the SQL and you must `make db-reset`.** `schema.Apply` is a no-op
  against a warm container, so an edited schema file never reaches Postgres and
  the run silently tests the old one. The demo and control targets reset first
  for exactly this reason.
- **`make negative-controls` refuses a dirty working tree.** It edits tracked
  files and puts them back. Commit or stash first.
- **There is no seed data.** The schema creates no accounts. Every test seeds its
  own chart inside its rolled-back transaction.
- **A test that has to COMMIT gets a database of its own.** `testdb.Disposable`
  creates one, applies the schema, and drops it at the end. Whether two
  transactions can both take one idempotency key cannot be asked inside a single
  rolled-back transaction, and committing into the shared `ledger_test` moves the
  counts `TestTheAccountListIsEveryAccountInCodeOrder` and
  `TestTheTrialBalanceIsServedPerCurrency` assert on.
- **One transaction per test, never two.** Every seed writes the same account
  codes and `code` is `UNIQUE`, so a second `testdb.Tx(t)` inserting them waits on
  the first, which stays open until the test ends. The test then blocks until the
  package timeout and the whole suite reads as hung. `serveTx` exists to stop
  that.
- **A statement that raises takes the whole transaction with it.** Postgres puts
  a transaction into the aborted state after any error, so every read after it
  fails with `current transaction is aborted`. That is why `Balances` asks
  whether a kind exists before it filters on one, and why `Open` works inside a
  `SAVEPOINT`. `TestARefusedFilterLeavesTheTransactionUsable` holds the line.
- **The version stamp is checked against git.** `make build` stamps
  `git describe`, and `cmd/pacioli`'s test builds through the Makefile and holds
  the binary's output against the repository. It needs a real checkout: outside
  one the test fails rather than skips.
