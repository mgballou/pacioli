<div align="center">

<img src="docs/assets/mark.svg" width="64" alt="" />

<h1>pacioli</h1>

<p><strong>A double-entry ledger whose rules live in Postgres, not in the service in front of it.</strong></p>

<p>Five HTTP endpoints over a chart of accounts and an append-only book of postings.<br />
Balances are summed out of those postings on every read, so no stored total can drift.</p>

<p>The balance rule is a deferred constraint trigger, so a migration or a psql session<br />
cannot write around it, and a refusal says what you sent — not only the rule you broke.</p>

<p>
<a href="https://github.com/mgballou/pacioli/actions/workflows/ci.yml"><img src="https://github.com/mgballou/pacioli/actions/workflows/ci.yml/badge.svg" alt="CI" /></a>
<img src="https://img.shields.io/badge/go-1.26-2f5f96" alt="Go 1.26" />
<img src="https://img.shields.io/badge/license-MIT-2f5f96" alt="MIT" />
</p>

</div>

<br />

A ledger has nothing to photograph. This is the thing it can show instead: an entry
where the fee was typed as 15 rather than 150, and the refusal it earns.

```console
$ curl -sS -i -X POST localhost:8080/v1/transactions \
    -H 'Content-Type: application/json' \
    -H 'Idempotency-Key: 4f1c9a2e-deposit-0002' \
    -d '{"currency": "GBP",
         "description": "Customer deposit, less fee",
         "postings": [{"account": "assets.cash",          "amount_minor":  4500},
                      {"account": "liabilities.customer", "amount_minor": -4350},
                      {"account": "revenue.fees",         "amount_minor":   -15}]}'

HTTP/1.1 422 Unprocessable Entity
Content-Type: application/json

{
  "error": "the transaction does not balance",
  "net_minor": 135,
  "postings": 3,
  "expected": "postings that net to 0; debits are positive and credits negative",
  "see": "github.com/mgballou/pacioli — README.md and docs/DESIGN.md, or run `pacioli --help`"
}
```

Nothing was written. The key reservation and the entry are one transaction, so the key is
not burned either: correct the leg to -150, send it again under the same key, and it posts.

---

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

One posting is a `bigint`, so a single amount runs to ±9,223,372,036,854,775,807.
A total of them has no ceiling: a balance, a side of the trial balance and the
net an entry is refused with are all `numeric` in Postgres and arbitrary
precision in Go. There is no amount the book will take and then fail to add up.

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

**Both writes require `Content-Type: application/json`**, and anything else is
415. That is the CSRF control, and it is the whole of it. A cross-origin form or
image or `fetch` is sent without asking anybody first only while it declares one
of three content types — form-encoded, multipart, or text/plain. Requiring JSON
puts both writes outside that set, so a browser has to preflight, and the mux
answers `OPTIONS` with 405. `POST /v1/transactions` was already outside it,
because `Idempotency-Key` is a custom header, but that was an accident of the
retry contract rather than a decision. There is no authentication; see
`docs/DESIGN.md` for why not.

Three rules cover every refusal.

**A refusal says what you sent, not only which rule you broke.** Every one names
the parameter and hands back the value that broke the rule. Where the set of
allowed values is closed, `valid` lists all of it; where it is open but shaped,
`expected` says the shape in words. `see` is where the rule is written down.

```console
$ curl -s -X POST localhost:8080/v1/accounts \
    -H 'Content-Type: application/json' \
    -d '{"code":"assets.savings","name":"Savings","kind":"assets","currency":"GBP"}'
{
  "error": "no such account kind",
  "parameter": "kind",
  "value": "assets",
  "valid": [
    "asset",
    "liability",
    "equity",
    "revenue",
    "expense"
  ],
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

**Every quantity a client controls is bounded, and the refusal says the number.**
A code is at most 64 characters, a name 100, a description 500, an entry 1,000
legs. A value past one of those answers 422 with the rule in words, a trimmed
value, and `characters`: how long the value actually was, which a trimmed value
cannot say. The 1 MB request body is a backstop behind all of them, not the
bound — it is what let one entry carry 20,900 legs, 21 seconds inside a single
request, and four accounts answer a report with a megabyte.

## The schema

`internal/schema/0001_ledger.sql` is the whole of it, hand-written.

Postings and transactions are append-only. A mistake is corrected with a
reversing transaction. The balance triggers are `INITIALLY DEFERRED`, so an entry
can go in one leg at a time and still has to balance by the end of the
transaction.

A deferred check has to be `FOR EACH ROW` — Postgres has no deferred statement
trigger — so one hung straight off `postings` summed the whole entry once per
leg. A leg queues its transaction id in `balance_checks` instead, once, and the
deferred check hangs off that. The check is one sum however many legs arrive, and
it clears its own row so a leg added later queues another. `docs/DESIGN.md`
decision 17 has the measurements.

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

**Loopback, in a container and out of one.** The binary defaults to
`127.0.0.1:8080`, so running it puts nothing on the network. The image sets
`LEDGER_ADDR=0.0.0.0:8080` instead, because a published port forwards to the
container's network interface and cannot reach a process bound to the container's
own loopback. The address that decides who can reach the ledger is the host side
of the publish, and `compose.yaml` binds it to `127.0.0.1:8080`. A plain
`docker run -p 8080:8080` publishes on every interface; write
`-p 127.0.0.1:8080:8080` if that is not what you want.

**The deadlines.** Every one is a ceiling on something a client controls, and
each is settable by a flag or by `$LEDGER_<FLAG>` with the dashes as
underscores:

    -read-header-timeout 10s   a connection that opens and sends no headers
    -read-timeout        30s   the whole request off the wire
    -request-timeout     15s   the handler, and the database work behind it
    -write-timeout       45s   the socket, once the two above have been missed
    -idle-timeout       120s   a kept-alive connection carrying nothing

A request that runs past `-request-timeout` has its database work cancelled and
rolled back, and is answered 503 once its body has arrived; a client still
uploading is bounded by `-read-timeout` instead, because Go will not answer a
request it has not finished reading. A write cut off either way has written
nothing and can be sent again under the same idempotency key. Zero is refused,
because zero is what Go reads as no deadline at all.

Every connection to Postgres carries `statement_timeout=20s`, `lock_timeout=10s`
and `idle_in_transaction_session_timeout=30s`. Set one of those on the connection
string to move it. The reasoning for all eight numbers is `docs/DESIGN.md` 18.

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

A test that cannot fail is decoration. `negative-controls/` holds forty-seven declared
mutations — remove the unique index on idempotency keys, drop the balance
trigger, stop the server draining, set every timeout back to zero — each naming
the test that should catch it.

    make negative-controls

applies each mutation, runs its test, and insists the test is green before the
mutation and red after. Green after names a test that proves nothing. Red before
names a broken setup reading as a working guard. Both fail the run.

CI runs what a laptop runs, and nothing else: `fmt`, `build`, `vet`, `lint` and `test`
in one job, `make negative-controls` in a second.

## License

MIT. See `LICENSE`.
