# pacioli

A double-entry ledger service in Go. Postgres holds the accounting rules; the
API is standard library.

    docker compose up

Then, in another terminal:

    curl -s localhost:8080/v1/trial-balance

Named for Luca Pacioli, who published the first written account of double-entry
bookkeeping in Venice in 1494.

## What it does

A chart of accounts, an append-only book of postings, and balances derived from
those postings on every read. Five HTTP endpoints open accounts, take
transactions, and answer questions about the book.

Amounts are signed minor units. A debit is positive and a credit is negative, so
asking whether a transaction balances is one sum against zero. Postgres holds
that sum in a constraint trigger deferred to the end of the transaction. No
caller can commit an entry that does not balance.

The only dependency is `pgx`. Only its `database/sql` driver registration is
imported, and every query goes through `database/sql`. There is no ORM and no
migration framework. One hand-written SQL file is the whole schema.

`docs/DESIGN.md` gives the reasoning behind each of these choices.

## The endpoints

```
GET  /v1/accounts          every account, in code order; ?currency= and ?kind= narrow it
POST /v1/accounts          open one — code, name, kind, currency; a code already
                           held answers 409 and never opens a second account
GET  /v1/accounts/{code}   one account's position
GET  /v1/trial-balance     debits and credits per currency, and whether they cancel
POST /v1/transactions      a balanced entry in, the transaction it became out;
                           Idempotency-Key is required
```

All JSON. Routing is `http.ServeMux` with the method in the pattern, so a `GET`
on `/v1/transactions` answers 405 and carries the `Allow` header the mux worked
out. There is no router dependency.

Three rules cover every refusal.

**A refusal says what you sent, not only which rule you broke.** Every one names
the parameter and hands back the value that broke the rule. Where the set of
allowed values is closed, `valid` lists all of it; where it is open but shaped,
`expected` says the shape in words. `see` is where the rule is written down.

```
$ curl -s -X POST localhost:8080/v1/accounts \
    -d '{"code":"assets.savings","name":"Savings","kind":"assets","currency":"GBP"}'
{
  "error": "no such account kind",
  "parameter": "kind",
  "value": "assets",
  "valid": ["asset", "liability", "equity", "revenue", "expense"],
  "see": "github.com/mgballou/pacioli — README.md and docs/DESIGN.md, or run `pacioli --help`"
}
```

The closed sets are read out of Postgres at the point of failure, so a kind added
to the schema is listed with no edit in Go. `see` is one constant in
`internal/docs`; there is no documentation site yet, and when there is one that
constant is the only line that changes.

**A closed set gets a refusal. An open set gets an empty answer.** The five
account kinds are closed, so `?kind=liabilty` answers 400. Currencies are open,
so `?currency=ZWL` answers 200 over `[]`. The query parameters an endpoint
defines are closed too, so `?curency=GBP` answers 400 and names the parameter it
could not read.

**400 means the request could not be read. 422 means the ledger refused it.**
Bad JSON, a field the endpoint does not define, a field of the wrong type: 400,
and the ledger is never asked. An entry that does not balance, a leg naming an
account the chart does not hold, a leg in another currency: 422, and every one of
those is Postgres refusing. No validation branch in Go decides either status.

An unbalanced entry comes back with the arithmetic of what was sent: how far out,
over how many legs.

## The schema

`internal/schema/0001_ledger.sql` is the whole of it, hand-written.

Postings and transactions are append-only. A mistake is corrected with a
reversing transaction. Both balance triggers are `INITIALLY DEFERRED`, so an
entry can go in one leg at a time and still has to balance by the end of the
transaction.

## Writing to it

`internal/ledger` is the write path. `Open` puts an account in the chart. `Post`
puts an entry in the book.

```go
err := ledger.Open(ctx, tx, ledger.Account{
    Code: "assets.cash", Name: "Cash at bank", Kind: "asset", Currency: "GBP",
})

id, err := ledger.Post(ctx, tx, ledger.Entry{
    Currency:    "GBP",
    Description: "Customer deposit, less fee",
    Legs: []ledger.Leg{
        {Account: "assets.cash", AmountMinor: 4500},
        {Account: "liabilities.customer", AmountMinor: -4350},
        {Account: "revenue.fees", AmountMinor: -150},
    },
})
```

Both take a `*sql.Tx`. The legs of an entry are only ever true together, and a
chart of accounts goes down whole.

`Post` checks nothing itself. It writes, settles the deferred balance check, and
translates whatever Postgres says into `ErrUnbalanced`, `ErrUnknownAccount`,
`ErrCurrencyMismatch` or `ErrRejected`. Settling early is what lets a refusal
name the entry that caused it. A refused entry leaves nothing behind and leaves
the caller's transaction usable.

## Reading it

Balances come out of the postings every time something asks for them. Nothing is
stored, so nothing can drift.

```go
b, err := ledger.BalanceOf(ctx, tx, "assets.cash")            // one account
all, err := ledger.Balances(ctx, tx, ledger.AccountFilter{})  // every account, in code order
trial, err := ledger.TrialBalance(ctx, tx)                    // debits and credits, per currency

usd, err := ledger.Balances(ctx, tx, ledger.AccountFilter{Currency: "USD", Kind: "asset"})
```

Postgres computes every number in one statement, on the caller's own
transaction. The answer covers the caller's unfinished work and everything
anyone else has committed, at one snapshot.

A trial balance is per currency. Pence and cents in one total is a number with no
meaning.

## The retry contract

`POST /v1/transactions` requires an `Idempotency-Key` header. A request without
one answers 400 and the ledger is never asked. An endpoint whose default is a
double posting makes the wrong thing easy, and the client least likely to send a
key is the one that most needs it.

The second request under a key gets the first request's answer: the same 201, the
same body byte for byte, and an `Idempotent-Replayed: true` header to say what
happened. The body is read back out of the ledger on the same rule balances
follow.

The same key with a different request answers 409. "Different" is a SHA-256 of
the request as the endpoint read it and re-encoded, so whitespace and field order
do not count.

**The concurrency is the primary key on `idempotency_keys`.** Two requests
carrying one key can arrive in the same instant. The reservation is an `INSERT`
that runs before the entry is written, so the second request blocks on the index
until the first transaction commits or rolls back, and then finds the key taken
or takes it. A duplicate does no work at all.

An entry the ledger refuses does not burn its key. The reservation and the entry
are one transaction, so a client can correct the body and send it again under the
same key.

Two things are missing. Keys are global, because nothing here knows which client
sent one. Nothing expires them, so the table grows forever.

## Running it

`docker compose up` builds the binary, starts Postgres on a volume, and serves on
`127.0.0.1:8080`. From a checkout instead:

    make run      # start the database, build, and serve on 127.0.0.1:8080

`LEDGER_ADDR` and `LEDGER_DSN` set the listen address and the connection string.
`make run ADDR=127.0.0.1:9000` moves the port.

The ledger is empty until something opens an account on it. There is no seed data
anywhere in this repository. Ctrl-C stops the server, and it finishes the requests
it has already accepted before it exits.

## Seeing it work

    make demo

One command. It empties the database, starts the server on it, and prints seven
steps: an empty ledger, three accounts opened over the API, a deposit taken, the
same deposit with a leg mistyped and refused, the deposit sent again under its key
with nothing written, the trial balance, and a server that finishes what it
accepted before it stops. Every step goes over HTTP.

```
make demo-post           # a transaction taken and one refused, with the balances either side
make demo-idempotency    # one key sent twice, then sixteen of it at once under -race
make demo-serve          # the binary on a socket, four exchanges, a clean exit
```

## Running the tests

```
make all      # build, vet, staticcheck, race tests
make test     # race tests only; starts the database itself
make db-down  # stop the test database and delete everything in it
```

Needs Go 1.26 and a container runtime. Tests run against real Postgres and never
a mock. Each test gets a transaction that is rolled back when it ends, so order
cannot matter. See `internal/testdb`.

The test database is Postgres 18 on `127.0.0.1:55432`, described in
`compose.test.yaml`. Its data directory is a tmpfs, so it starts empty every run.

## Proving the tests can fail

A test that cannot fail is decoration. `negative-controls/` holds twenty-seven
declared mutations — remove the unique index on idempotency keys, drop the
balance trigger, stop the server draining — each naming the test that should
catch it.

    make negative-controls

applies each mutation, runs its test, and insists the test is green before the
mutation and red after. Green after names a test that proves nothing. Red before
names a broken setup reading as a working guard. Both fail the run.

## License

MIT. See `LICENSE`.
