# Design

Twenty-two decisions, each in the same shape: what it does, the obvious
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

## 15. One posting is fixed-width; a total of postings is not

**What it does.** `amount_minor` is `bigint`, so a single posting runs to
±9,223,372,036,854,775,807 and no further. Every *total* of postings is
`numeric`: an account balance, either side of the trial balance, and the sum the
balance trigger takes. In Go each of those is a `ledger.Minor`, an
arbitrary-precision integer that scans a `numeric` off the row and writes itself
back out as a JSON number. A `Leg` stays an `int64`, because that is what one
posting is. No float appears on either side.

The trigger sums into a `numeric` as well, so an entry whose legs sum past
`bigint` is refused for the reason it is wrong — it does not balance — and told
by how much. Decision 14 asks that of every refusal: where the rule broken is
arithmetic, it is the arithmetic that comes back. This was the one refusal in the
repo that did not, because summing into a `bigint` raised a range error the
handler could say nothing about.

**The obvious alternative.** Two of them.

*Refuse at write time.* Keep `int64` everywhere and check the running total
before each posting lands, so a balance can never leave the range Go can read.

*Keep `int64` and widen with a library.* An `int128` or a decimal package in the
money path, `numeric` underneath.

**Why those are wrong.** Refusing at write time is the cheaper answer and it
does not work here, for two reasons that are worth separating.

The first is that it does not cover the report that broke. A per-account ceiling
bounds a balance, and `debits` on the trial balance is `sum(...) FILTER (WHERE
amount_minor > 0)` — a total that only ever grows. A reversing entry adds to it.
So the only check that would bound the trial balance is a ceiling on every
posting ever written in a currency, which is a different and much larger claim
than "this account holds too much".

The second is the cost, measured. Against one million postings on the repo's own
`compose.test.yaml` — tmpfs, `fsync=off`, loopback — one ordinary two-leg entry
takes about 3.6 ms end to end. The per-account running total takes 17–27 ms and
the per-currency debits total 28–30 ms. Both plan as a parallel sequential scan
of the whole `postings` table, so both grow with the book and both take more
than one backend per write. And neither is correct on its own: two writers each
read a total under the ceiling, each write, and together cross it, so the check
also needs a lock held for the length of the write. That is five to eight times
the cost of the write it guards, rising, against a range nobody reaches by
accident.

A library is the same answer as `numeric` with a dependency added to the one
path that has none. `math/big` is in the standard library, the arithmetic here is
addition and comparison, and the driver already hands a `numeric` over as its
digits. There is nothing left for a package to do.

A ceiling that refuses would have been a legitimate answer for a ledger. What is
not legitimate is what the code did instead: take the entry, hold the right
number, and then fail to read it back. Postgres never lost the answer. Only the
scan type could not hold it, and `GET /v1/trial-balance` could not be brought
back by any entry a person could write, because the book is append-only and the
sum only grows. The bug was that a report could not be repaired.

**What this does to a book that already exists.** For the read path, nothing:
the totals were always `numeric` and the fix is on the Go side of the wire, so a
ledger already past the ceiling starts answering again as soon as the new binary
runs. No migration, no rewrite, no downtime. The one schema change is the
`numeric` in `assert_transaction_balances`, and it only improves a refusal. A
database that already holds the schema takes it as a single statement:

    CREATE OR REPLACE FUNCTION assert_transaction_balances(txn_id uuid) RETURNS void
    LANGUAGE plpgsql AS $$
    DECLARE
        legs bigint;
        -- numeric, not bigint: two legs the column will each hold can sum past one.
        net  numeric;
    BEGIN
        SELECT count(*), coalesce(sum(amount_minor), 0)
          INTO legs, net
          FROM postings
         WHERE transaction_id = txn_id;

        IF legs = 0 THEN
            RAISE EXCEPTION 'transaction % has no postings', txn_id
                USING ERRCODE = 'LB001',
                      HINT = 'A transaction needs at least two postings summing to zero.';
        END IF;

        IF net <> 0 THEN
            RAISE EXCEPTION 'transaction % does not balance: % postings net to % (want 0)',
                txn_id, legs, net
                USING ERRCODE = 'LB001',
                      HINT = 'Debits are positive, credits negative; the sides must cancel.';
        END IF;
    END;
    $$;

which replaces the function body and touches no row. `internal/schema` applies
`0001_ledger.sql` only to a database that does not have it yet, so an existing
book needs that one statement run by hand. There is no migration framework and
this did not earn one.

## 16. Every quantity a client sends is bounded, and the bound is a number

**What it does.** A code is at most 64 characters, a name 100, a description
500, and an entry 1,000 legs. The three lengths are `CHECK`s in the schema; the
leg count is a refusal in `internal/ledgerhttp`. Each refusal names the rule in
words, hands back a trimmed value, and says how long the value actually was.

**The obvious alternative.** The 1 MB request body already caps all of it. Lower
it and be done.

**Why that is wrong.** A body limit is a backstop, not a bound. It says nothing
about any one field, so every field inherits the whole of it: a description ran
to a megabyte, an account code of 100 kB came back out as a URL path segment, and
four accounts answered `GET /v1/accounts` with 1,000,655 bytes because a report
reads every name back in full. Lowering the body limit moves that number without
naming any of them, and it breaks a legitimate large entry to fix a description.

Every guard in this repo was about meaning — does it balance, does the currency
match, is the kind real — and none was about size. Meaning and size are different
questions and a rule that answers one does not answer the other.

The numbers themselves:

- **64 characters for a code.** It is a URL path segment and a report heading.
  `assets.cash.operating.gbp` is 25. The shape was always a character class, so
  the length goes inside the same regex and stays one constraint, which is what
  lets a refusal read the field name off the constraint name.
- **100 for a name.** It is a line on a trial balance. Anything longer is a
  description that has gone in the wrong field.
- **500 for a description.** A memo line, not a document. Long enough for a
  sentence and a reference, short enough that a list of entries stays readable.
- **1,000 legs for an entry.** Most entries have two. A consolidated payroll or
  settlement journal has hundreds. A thousand is past all of them, and it is the
  number that holds one request to a thousand round trips and a response of tens
  of kilobytes. The 1 MB body was allowing 20,900 of the legs this repo's own
  demo writes, and one such request took 21.6 seconds end to end.

The lengths are written twice — once in the `CHECK`, once in `internal/ledger` so
a refusal can say them — and a test reads `pg_get_constraintdef` back and fails
if the two ever disagree.

**What this does to a book that already exists.** A book already holding a value
past one of these bounds keeps it: the `CHECK`s are on the write path and nothing
rewrites a row. Adding them to an existing database needs the rows to pass first,
so it is three statements and a look at what they refuse:

    SELECT code FROM accounts WHERE code !~ '^[a-z][a-z0-9_.]{0,63}$';
    SELECT code FROM accounts WHERE char_length(name) > 100;
    SELECT id   FROM transactions WHERE char_length(description) > 500;

and then the `CHECK`s themselves. There is no migration framework and this did
not earn one.

## 17. The balance check is queued once for an entry, not once per leg

**What it does.** A posting queues its transaction id in `balance_checks`, once,
by primary key conflict. The deferred constraint trigger hangs off *that* table,
sums the entry once, and deletes the row it fired on.

**The obvious alternative.** The constraint trigger on `postings`, which is what
it was.

**Why that is wrong.** It was `FOR EACH ROW DEFERRED`, so an *n*-leg entry queued
*n* checks and each one summed *n* postings. The cost of writing an entry was
quadratic in its legs, and the only ceiling was the request body:

| legs | write, before | check, before | write, after | check, after |
|---|---|---|---|---|
| 1,000 | 16 ms | 63 ms | 22 ms | 1.0 ms |
| 2,000 | 30 ms | 208 ms | 39 ms | 0.4 ms |
| 4,000 | 56 ms | 829 ms | 86 ms | 0.8 ms |
| 8,000 | 116 ms | 2,964 ms | 170 ms | 1.3 ms |
| 16,000 | 239 ms | 12,573 ms | 350 ms | 1.8 ms |

Postgres 18.6, the container in `compose.test.yaml`, legs written in one
statement, `SET CONSTRAINTS ALL IMMEDIATE` timed on its own. The check is now
flat: one sum, whatever the leg count. The write is about half as much again,
because every leg now probes one primary key, and that is the trade — a bounded
cost per leg for a cost per entry that no longer squares.

**The obvious fix, and why it is not available.** Make the trigger
`FOR EACH STATEMENT` and keep it deferred. Postgres will not take it:

    CREATE CONSTRAINT TRIGGER t AFTER INSERT ON postings
        DEFERRABLE INITIALLY DEFERRED FOR EACH STATEMENT ...
    ERROR:  syntax error at or near "STATEMENT"

`CREATE CONSTRAINT TRIGGER` takes `FOR EACH ROW` and nothing else, it will not
take a `REFERENCING` transition table either, and a plain `CREATE TRIGGER` will
not take `DEFERRABLE`. Deferral and statement scope cannot be had on the same
trigger. So the deferred check moves to a table that already has one row per
entry, and a plain immediate row trigger on `postings` is what puts it there.

It would not have helped anyway: `ledger.Post` writes one statement per leg, so
that it can name the leg a refusal was about. A statement trigger would have
fired once per leg too.

**Why the queue row is deleted.** So that legs added after a check queue another
one. Without the delete, an entry could be settled and then quietly unbalanced by
an append. The row never outlives the transaction that wrote it, and the entry
with no legs at all still has `transactions_must_balance` over it, because an
entry with no legs queues nothing.

## 18. Every deadline is a number, and none of them is zero

**What it does.** The listener carries `ReadHeaderTimeout`, `ReadTimeout`,
`WriteTimeout` and `IdleTimeout`; every request carries a budget on its context;
and every connection to Postgres carries `statement_timeout`, `lock_timeout` and
`idle_in_transaction_session_timeout`. Each of the five server deadlines is
settable by a flag or by `$LEDGER_<FLAG>`, and a value of zero or less is
refused. The three database ceilings are settable on the connection string,
which keeps its own value where it names one.

**The obvious alternative.** Leave them out. Go's zero value is no deadline, and
`ReadHeaderTimeout` alone stops the slow-headers attack everybody has heard of.

**Why that is wrong.** Every one of them is a ceiling on something a client
controls, and there were none. A connection that opened and dribbled a body sat
there. A connection that went quiet after one request sat there. A client that
stopped reading left a handler blocked in `Write` forever. And a pool of sixteen
connections, held by writers with no deadline, is a read queueing behind them for
as long as they take: two hundred concurrent maximum-size writes put
`GET /v1/accounts` at 7.4 s against 5 ms idle, and nothing refused anything.
Decision 16 took the worst of that away by bounding the legs of an entry — the
same reproduction was 80 seconds before it — but bounding the work is not the
same as bounding the wait. None of this is an attack; it is a bad network and a
busy afternoon.

The numbers, and why each is the number it is:

| deadline | value | what it bounds |
|---|---|---|
| `-read-header-timeout` | 10s | a connection that opens and sends no headers. The headers here are a few hundred bytes. |
| `-read-timeout` | 30s | the whole request off the wire. The body is capped at 1 MB, so this is 1 MB at about 280 kbit/s. |
| `-request-timeout` | 15s | the handler, and the database work behind it. |
| `-write-timeout` | 45s | the socket, once the two above have already been missed. It is read plus request, so it can only fire as a backstop. |
| `-idle-timeout` | 120s | a kept-alive connection carrying nothing. |
| `statement_timeout` | 20s | one statement. Above the request budget, so the budget is the normal path and this is what holds when a cancellation does not arrive. |
| `lock_timeout` | 10s | one lock wait. Above the slowest measured write and below the request budget. |
| `idle_in_transaction_session_timeout` | 30s | an open transaction with nothing happening on it. Twice the request budget, so it can only fire after the budget already has. |

**Fifteen seconds, and where it came from.** The slowest thing the ledger
legitimately does is a 1,000-leg entry — the ceiling `maxPostings` sets. Sixteen
of those at once take 0.6 s each; two hundred at once take 3.5 s each. Fifteen
seconds is four times the worst measured, so a request that reaches it is
queueing rather than working, and the answer it deserves is a refusal.

**The budget is on the context, not only on the socket.** `WriteTimeout` closes a
connection; it does not stop the handler behind it, and a handler waiting on
Postgres would go on holding one connection out of sixteen for a client that has
already gone. `ledgerhttp.Deadline` is what makes the deadline reach the
database: it puts the budget on the request's context, the transaction is
cancelled and rolled back, and the connection goes back to the pool.

**A write cut off mid-flight writes nothing.** The reservation, the entry and the
read-back are one transaction, so a cancelled write rolls the idempotency key
back with it and the client can send the same request again under the same key.
Four hundred concurrent 1,000-leg entries against a pool of sixteen: 327 taken,
73 refused at the budget, and afterwards 327 transactions, 327,000 postings
netting to zero, 327 keys all settled, no unbalanced entry and no leftover
balance-check row. The deadline is a refusal, not a half-written entry.

**A cancelled request is a refusal, not a fault.** `Deadline` usually answers
first and the handler's own body is thrown away, but a handler that finishes a
moment before its budget hands its body to the client — and under four hundred
concurrent writes, one in four hundred did. `context.Canceled` and
`context.DeadlineExceeded` are therefore 503 rather than the 500 every unnamed
error gets, and they are not logged as the cause behind one, because there was
no fault to record.

`driver.ErrBadConn` is answered the same way, and it is the deadline's own
wake. Cancelling a query is what leaves a pooled connection unusable;
`database/sql` retries on fresh ones and hands this back only when it runs out
of them. Under a deliberately punishing two-second budget — 342 of 400 requests
cancelled — one request got it. It means the transaction was never begun, so
nothing was written and the answer the client deserves is "send it again",
which for a write is safe because the idempotency key is still free.

**What the budget cannot do, and why `-read-timeout` is not redundant.** Go will
not put a response on a connection whose request body is still arriving, and a
handler blocked reading that body is not interrupted by its own cancelled
context. Measured against a two-second budget and a client sending one byte every
half second: the context was cancelled at 2.0 s, and the handler stayed inside
the body read until the socket closed at 12.0 s. So the budget bounds the work
and the database, and `-read-timeout` is the only thing that bounds a client that
never finishes sending. Both are needed, and neither covers the other.

**Zero is refused rather than kept.** A deadline of zero is what Go reads as no
deadline, so a flag that took it would put the server back where it started and
look like configuration while doing it.

## 19. A read runs at repeatable read; a write runs at read committed

**What it does.** `Pool.Read` begins `REPEATABLE READ READ ONLY`. `Pool.Write`
begins `READ COMMITTED`, said out loud rather than inherited from the connection.

**The obvious alternative.** Set neither and take what the connection gives,
which is read committed for both. That is what it did.

**Why that is wrong, for the read.** `Pool.Read` says a response built from two
statements is built from one snapshot of the ledger, and at read committed that
is not true: every statement takes its own snapshot. `Balances` is two statements
— it reads the `account_kind` enum, then the balances — and a write landing
between them is a response half from one ledger and half from another. Repeatable
read is what makes the claim the comment already made.

**Why that is wrong, for the write.** The write is the interesting one, because
the level it needs is the weaker of the two and nothing said so. The idempotency
replay reads a row that another transaction committed *after* this one began: a
duplicate key blocks on the primary key, the holder commits, this one is refused
with 23505, and only then does it read back the result to replay. At repeatable
read that read comes back empty — the snapshot was taken before the other
transaction committed — and every duplicate would be answered with a 500 saying
the key was taken and is already gone. Measured against Postgres 18.6, the
container in `compose.test.yaml`:

| level | the duplicate insert | the read back |
|---|---|---|
| read committed | 23505 | the first call's row |
| repeatable read | 23505 | **0 rows** |

So the retry contract, which is the point of the endpoint, depends on read
committed. It now says so, and a control turns the tests red if it stops saying
so.

**What the deferred balance check depends on, and it is not the level.** The
check sums the postings of one transaction id from inside the transaction that
wrote them, so it needs to see its own uncommitted writes and nothing else — true
at every isolation level Postgres offers. Nothing else can add legs to that
entry: the id is generated inside the writing transaction and never leaves it
before commit, and postings are append-only. The balance rule is safe at any
level. The retry contract is not.

## 20. A refusal never answers with a word that is true of what was sent

**What it does.** The two writes read a body a field at a time. A field that is
not the shape the endpoint takes is named, with the json the body held there and
the rule it broke, and every field a body got wrong is named rather than the
first:

```console
$ curl -s -X POST localhost:8080/v1/transactions \
    -H 'Content-Type: application/json' -H 'Idempotency-Key: 4f1c9a2e-lie-0001' \
    -d '{"currency":"GBP","occurred_at":"tuesday","description":"x","postings":[]}'
{
  "error": "the field is not the shape this endpoint takes",
  "parameter": "occurred_at",
  "value": "\"tuesday\"",
  "expected": "an RFC 3339 timestamp, as in 2026-09-07T14:30:00Z",
  "see": "..."
}
```

`"amount_minor": 100.5` answers `a whole number of minor units, so 100.50 is
10050, from -9223372036854775808 to 9223372036854775807`, and hands back `100.5`
as the value. It used to answer `expected: number`.

**The obvious alternative.** Hand the whole body to `encoding/json` and report
whatever it returns, which is what it did. That is two lines per endpoint and it
reads every shape the wire types can hold, so nothing about a field has to be
written down twice.

**Why that is wrong, for the amounts.** 100.5 *is* a number. A message that
names the rule the value already satisfies tells a client its own value was the
one that was asked for, which is worse than saying nothing: it sends the reader
to look at the field name, the nesting, the content type — anywhere but the
decimal point. The rule that was actually broken is this ledger's, not json's,
and json cannot state it, because json has one kind of number and no idea what a
minor unit is.

**Why that is wrong, for the rest.** `encoding/json` reports the field it could
not read only where the failure is its own. A type that reads itself reports a
plain error carrying no field at all, and `occurred_at` is a `time.Time`, so a
body holding `"tuesday"` came back **the request body is not json this endpoint
can read** — of a body that was good json throughout. The same sentence covered
every body that was json and was not an object: an array, a bare string, a
number, and `null`, which `json.Unmarshal` reads into a map without complaint
and which therefore reached the ledger as an empty entry and was refused for a
missing currency the client never sent. Four false sentences, on the first thing
a reader testing the README's headline claim will type.

And the decoder stops at the first fault it finds, so a body with three mistakes
in it took three round trips to mend, each one revealing the next.

**What the reading owes.** `readValue` dispatches on the type: a type that reads
itself is a leaf and its refusal is turned into the field's name; a struct is
read key by key against its own json tags; a slice is read element by element,
so a fault in the third leg is named `postings[2].amount_minor` and not
`postings.amount_minor`. A name the object does not define is refused against
that object's own set, which is why a misspelt field inside a posting is now
offered `account` and `amount_minor` rather than every name the endpoint takes
at any depth.

`shaped` is one map from a json field name to the rule in words, read by both
endpoints because both decode through the same reader. It is the same shape as
`bounded`, which does the same job for the fields the schema holds to a length.
Two entries, `amount_minor` and `occurred_at`, and that is the point: the fix is
not a string edited in one branch of one handler.

**What the value is.** The json the body held, which is not the same as the value
a schema refusal hands back: that one is the string the ledger was given, after
this reader decoded it. A string keeps its quotes here, because without them the
number 100 and the string `"100"` are refused with the same four characters and
this endpoint takes one of them.

**What stays.** A body that is genuinely not json is still told so, and still
told where the reading stopped. The status codes are the ones these refusals
already used. A body that got one field wrong is answered in the shape it always
was — the name, the value and the rule at the top level — and `fields` appears
only when there is more than one, carrying all of them, the first included.

## 21. Blank is a rule in the schema, and it is written once

**What it does.** `is_blank(s)` is true when `s` holds no character that is not
whitespace. `has_control_character(s)` is true when it holds one anywhere. Both
are `IMMUTABLE` SQL functions in `0001_ledger.sql`, and the `CHECK` on
`accounts.name` and on `transactions.description` asks both. `internal/ledger`
says the same rule in one sentence, `NotBlank`, which `NameShape` and
`DescriptionShape` are built out of.

**The obvious alternative.** `btrim(x) <> ''`, which is what it was, and which
reads exactly like a not-blank check.

**Why that is wrong.** `btrim()` with no second argument trims spaces. Not tabs,
not newlines, not carriage returns. So a description of one newline was a
description, a name of one tab was a name, and the check that was there to stop
an empty label stopped only one of the six ways to write one. Every field of a
person's words had the same hole, because they all had the same check.

Control characters are refused wherever they appear and not only alone, because
a name is read back into a report and a description into a log line, and neither
is a place to put an escape sequence. The account code, the currency and the
idempotency key need none of this: each is a closed set of characters —
`^[a-z][a-z0-9_.]{0,63}$`, `^[A-Z]{3}$`, `^[[:graph:]]{16,255}$` — and a closed
set already excludes what these two functions exclude. Postgres anchors `^` and
`$` to the whole string rather than to a line, so `assets.cash\n` is refused by
the code shape, and a test says so rather than a comment claiming it. The code is
the sharp one: it comes back out as a URL path segment.

**Why in the schema rather than in Go.** The same reason the balance rule is
there. A rule in the handler is a rule a `psql` session walks around, and this
one guards what a report and a log line will carry.

## 22. A posted transaction has an address

**What it does.** `POST /v1/transactions` answers with a transaction id and a
`Location` header, and `GET /v1/transactions/{id}` serves the entry that id
names, in the shape the write answered with. An id that is a uuid the book does
not hold is 404. An id that is not a uuid is 400, carrying the shape an id takes.

**The obvious alternative.** Stop returning the id, and say that a posted entry
is addressed by its idempotency key. The key already resolves — a replay under
it answers with the whole entry — so the retry contract was already the way back
to a write, and the id was a second one that did not work.

**Why that is wrong.** The key is the client's own string and only the client
that chose it can use it, so a system where one service posts and another reads
has nothing to pass between them. The id is the ledger's, it is already in the
answer, `EntryOf` already reads an entry back by it because that is how a replay
is answered, and the data is a primary key lookup and a join. The endpoint is
four lines of handler over a function that was already written and already
tested.

What was wrong was neither answer on its own. It was offering a handle to
nothing: a caller stored the id, came back, and found there was no endpoint to
come back to. `POST /v1/accounts` had carried a `Location` since it was written
and `POST /v1/transactions` had a comment saying it could not, which is the
smallest possible version of the same complaint.

**What it does not serve.** `occurred_at` goes in and does not come back, on
either the write or the read, because the two answer with the same body and
widening it is an API change this decision did not need to make.
