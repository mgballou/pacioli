# Design

Fourteen decisions, each in the same shape: what it does, the obvious
alternative, and why that alternative is wrong. The code carries almost no
argument in its comments. This is where the argument lives.

---

## 1. Amounts are signed minor units

**What it does.** A posting carries `amount_minor bigint`. A debit is positive
and a credit is negative. Asking whether a transaction balances is one sum
against zero.

**The obvious alternative.** A decimal amount plus a `direction` column holding
`debit` or `credit`, or a floating-point amount in a major unit.

**Why that is wrong.** A float cannot hold 0.10, so a book kept in floats drifts
by fractions of a cent and the drift compounds over every posting. Two columns
give every value two spellings. A credit of 500 and a debit of -500 are the same
money, the database will hold both, and the balance rule becomes a conditional
sum that has to agree with a column no constraint covers. One signed integer
gives each value one spelling. It also makes the whole of double-entry a
`SUM(...) = 0`.

## 2. The balance rule lives in Postgres

**What it does.** A constraint trigger on the transaction sums its postings and
raises if the sum is not zero. `internal/ledger` has no validation branch at all.

**The obvious alternative.** Check the sum in `ledger.Post` before the insert.

**Why that is wrong.** A check in Go binds the callers that go through it and
nothing else. A migration, a repair script, a `psql` session, a second service
written next year, a handler that takes a shortcut: each one writes around it.
The database is the only place every writer has to pass. A rule that can be
bypassed is a convention. A ledger with no rule it can enforce is a table of
numbers.

## 3. Balances are derived on every read and never stored

**What it does.** `ledger.Balances`, `ledger.BalanceOf` and
`ledger.TrialBalance` sum the postings in Postgres, in one statement, at read
time. No account carries a running total.

**The obvious alternative.** A `balance` column on `accounts`, updated as part of
each post.

**Why that is wrong.** A stored total is a copy of something the book already
knows, and any copy can come to disagree with its source. Nothing announces the
disagreement. The column answers, and the answer is wrong. Repairing it needs a
job that recomputes from the postings, which is the query this decision keeps
anyway. Postings are append-only, so the sum is reproducible from the book
forever. The cost is an index scan. One answer is worth that.

## 4. A trial balance is per currency and never across

**What it does.** `TrialBalance` groups by currency and reports debits, credits
and whether they cancel, for each currency separately.

**The obvious alternative.** One grand total, converting each currency at some
rate.

**Why that is wrong.** A rate belongs to a moment and to a source, and this book
holds neither. A total that mixes pence and cents means nothing. An auditor
will still read it as the answer, so this ledger does not produce one. Conversion belongs to a layer that knows where its rates
came from.

## 5. `ledger.Post` takes a `*sql.Tx`, not a pool

**What it does.** Every function in `internal/ledger` takes the caller's
transaction as an argument and never opens one.

**The obvious alternative.** `Post(ctx, db, entry)`, opening and committing its
own transaction.

**Why that is wrong.** The caller almost always has more to do in the same unit
of work: reserve an idempotency key, post a second entry, write a row somewhere
else. A function that owns its transaction cannot join the caller's, so the
caller either gives up atomicity or writes `Post` again. Taking the transaction
also fixes what a read can see. A balance read on the caller's `*sql.Tx` covers
that caller's unfinished work together with everything anyone else has
committed, at one snapshot.

## 6. `Post` settles the deferred trigger before it returns

**What it does.** After the insert, `Post` runs `SET CONSTRAINTS ... IMMEDIATE`,
so the balance trigger fires on the call rather than at commit.

**The obvious alternative.** Leave the trigger deferred and let it raise at
`COMMIT`.

**Why that is wrong.** An error raised at commit names the transaction. A caller
that posted three entries is told one of them does not balance and cannot be told
which. Worse, the code that made the mistake has already returned. Settling early
puts the refusal on the statement that caused it, which is what lets the handler
answer with the arithmetic of the entry that was sent.

## 7. `Open` runs inside a `SAVEPOINT`

**What it does.** `ledger.Open` takes a savepoint, inserts, and rolls back to the
savepoint if Postgres refuses.

**The obvious alternative.** Insert and return the error.

**Why that is wrong.** Postgres aborts the whole transaction after any raised
error, and every refusal `Open` can make is a raised error: a code already held,
a kind that is not in the enum. Without the savepoint, one mistyped account kind
costs the caller every statement after it, and the caller reads
`current transaction is aborted` instead of `ErrUnknownKind`. The savepoint is
what makes a refusal survivable. A chart of accounts laid down in a loop needs
that.

## 8. A closed set gets a refusal; an open set gets an empty answer

**What it does.** `?kind=liabilty` answers 400. `?currency=ZWL` answers 200 over
`[]`. `?curency=GBP` answers 400 and names the parameter.

**The obvious alternative.** Filter on whatever came in and answer 200 with an
empty list when nothing matches. Ignore query parameters the endpoint does not
recognize.

**Why that is wrong.** A typo that reads as zero is a bug that looks like an
answer. `?kind=liabilty` over `[]` tells the caller this ledger holds no
liabilities, and the caller believes it. `?curency=GBP` ignored returns the whole
book and reads as a filter that worked. Where the server knows the full set, an
empty answer misstates the ledger. Where it does not — currencies are open, and
an account can be opened in one this code has never seen — empty is the true
answer.

## 9. The closed set is read from Postgres, not held in Go

**What it does.** `ledger.checkKind` asks `enum_range(NULL::account_kind)` rather
than comparing against a list in the source.

**The obvious alternative.** `var kinds = []string{"asset", "liability", ...}`.

**Why that is wrong.** Two copies of the same list drift apart, and this pair
drifts in the direction that hurts. Add a sixth kind to the schema and the
database accepts an account in it while Go answers 400 on a valid request. The
enum is already the definition, because it is the thing that refuses the insert.
Reading it keeps one definition and makes a schema change enough on its own.

`checkKind` asks before it filters rather than letting the cast fail. Casting a
bad kind raises, and a raise aborts the caller's transaction, so every read after
it answers `current transaction is aborted` instead of the refusal. It reads the
whole enum rather than asking whether one value is in it, so the refusal can list
the kinds there are.

## 10. 400 means the request could not be read; 422 means the ledger refused it

**What it does.** Bad JSON, an undefined field, a field of the wrong type: 400,
and `internal/ledger` is never called. An entry that does not balance, a leg
naming an account the chart does not hold, a leg in another currency: 422, and
every one of those is a Postgres refusal translated.

**The obvious alternative.** 400 for everything the client got wrong.

**Why that is wrong.** The two statuses carry different instructions. 400 says
change the request. 422 says the request was fine and the book will not hold what
it asked for. A client that cannot tell them apart cannot decide whether to fix
its serializer or its accounting, and an operator watching error rates cannot
tell a broken integration from a stream of business refusals. The line also keeps
the code honest: everything on the 422 side comes from the database, so there is
no second copy of the accounting rules in Go to disagree with the first.

## 11. The idempotency key is reserved by an `INSERT` before the write

**What it does.** `POST /v1/transactions` requires an `Idempotency-Key` header. A
request without one answers 400 and the ledger is never asked.

`ledger.Once` takes the key and the write as a function. It inserts the key into
`idempotency_keys` first, then runs the write inside the same transaction. The
reservation row holds no result yet, and a deferred trigger insists it holds one
by the time the transaction commits.

A second request under the same key blocks on the primary key until the first
transaction commits or rolls back. It then finds one of two things. If the key is
taken and the request is the same, it is a replay: the answer is read back out of
the ledger by `ledger.EntryOf` and returned as the same 201, the same body byte
for byte, and an `Idempotent-Replayed: true` header. If the key is taken and the
request is different, the answer is 409. "Different" is a SHA-256 of the request
as the endpoint decoded and re-encoded it, so whitespace and field order do not
count, and nothing else can differ because the endpoint refuses fields it does not
define.

**The obvious alternative.** `SELECT` the key. If a row comes back, return the
stored answer. If not, write the entry and `INSERT` the key.

**Why that is wrong.** Read-then-write leaves a window between the read and the
write. Two requests carrying one key can both find nothing and both post. Nothing
raises. No constraint is violated. The ledger simply holds two identical entries,
the client sent one payment, and the second one is real money that will have to be
found and reversed by hand.

The window is small. It is also the window a retrying client aims at. A retry
follows a timeout, a timeout follows load, and load is when two requests arrive
together. The race stays rare while the system is quiet and turns routine while
it is busy.

An `INSERT` closes the window because the index does the work. The second
transaction waits on the first instead of racing it, and Postgres decides the
order. Nothing takes a lock in Go, no advisory lock can leak, and there is no
window left to reason about. The primary key on `idempotency_keys` is the whole
of the concurrency control.

Reserving before the write pays twice. A duplicate does no work at all rather than
doing the work and throwing it away. And an entry the ledger *refuses* does not
burn its key, because the reservation and the entry are one transaction that rolls
back together, so a client can correct its body and send it again under the same
key.

The replayed answer is read back out of the ledger rather than stored beside the
key. That is decision 3's rule applied again: a stored copy of an answer is one
more thing that can come to disagree with the postings.

**What this does not buy.** Keys are global, because nothing in this ledger knows
which client sent one. Scoping them per client is what an identity layer is for.
Nothing expires them either, so `idempotency_keys` grows forever. Neither is
built. Both are written down here.

## 12. A handler takes a `Store`, not a `*sql.DB`

**What it does.** `ledgerhttp.Handler` is given a `Store` that hands out
transactions. `ledgerhttp.Pool` is the one a running server uses: a read gets a
`READ ONLY` transaction and is rolled back, a write gets its own and is committed
only if the handler got through. A test supplies a `Store` that hands the handler
the transaction the test is already inside.

**The obvious alternative.** Give the handler the `*sql.DB` and let it open what
it needs.

**Why that is wrong.** Postings are append-only and cannot be truncated, so a
handler that commits leaves rows behind for the life of the container. Every HTTP
test would then move the counts every other test asserts on, and the suite would
depend on the order it ran in. The `Store` seam is what lets an HTTP test roll
back like every other test here.

The seam pays a second time in production. Reads go through a `READ ONLY`
transaction, so Postgres refuses a write on a read path whatever a later change
to a handler does. The server enforces the rule. Nobody has to remember it.

## 13. A refusal is a value, not a message to be read

**What it does.** `internal/ledger` refuses with sentinels, and with two types
that carry what the sentinel cannot. `*LegError` holds the leg's index, its
account, its amount and the currency that account holds. `*UnknownKindError`
holds the kind that was given and the kinds the enum has. `internal/ledgerhttp`
picks a status with `errors.Is` and takes the closed set off the error with
`errors.As`. It never reads the text.

**The obvious alternative.** One error per package, built with `fmt.Errorf`, and
callers that match on what it says.

**Why that is wrong.** A message is written for a person, so it gets reworded —
by a better error message, by a lint rule, by a translation. Every reword breaks
a caller that was matching on it, and it breaks at run time, in the refusal path,
which is the path least likely to be exercised. A type breaks at compile time
instead. It also carries more than a sentence can: the HTTP layer hands a client
the five kinds the enum holds without this package ever spelling them out, so the
list in the answer and the list in the schema cannot come apart.

## 14. A refusal hands back the value that broke the rule, and never the schema's own words

**What it does.** Every refusal names the parameter, hands back the value the
request carried under it, and says what would have been taken — the whole set
where the set is closed, the rule in words where it is open. `shown` trims each
value to 80 runes. Where the refusal is arithmetic rather than a value the client
typed, it is the arithmetic that comes back: an unbalanced entry answers with how
far out it was and over how many legs. The server's own message goes to the log.

**The obvious alternative.** Answer with what Postgres said. It is accurate, it
is already written, and it names the constraint.

**Why that is wrong.** It names the constraint. `transactions_currency_check`
means nothing to a client and everything to anyone reading for a way in, and
translating it into a client-facing sentence means a copy of the schema in Go
that can disagree with the schema. `refusedField` takes only the column name out
of it — Postgres builds these as `<table>_<column>_check` — and gives nothing
back when that name is not a field the endpoint takes. A guess is worse than
silence.

Handing the value back is what makes a refusal actionable. A client that sent 400
values and gets "the ledger will not hold that account" has to bisect its own
request. One that gets `{"parameter": "currency", "value": "gbp"}` has the answer.
Nothing is handed back that the ledger would not serve anyway: a code collision
is told what holds the code, and `GET /v1/accounts/{code}` serves that to anyone.
And nothing is handed back untrimmed, because every one of these values arrived
over the wire and none of them is bounded.
