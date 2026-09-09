# pacioli

A double-entry ledger: six HTTP endpoints over a chart of accounts and an
append-only book of postings, with the accounting rules held as Postgres
constraints. Go 1.26, one dependency (`pgx`), one hand-written schema file.

Read these first:

- `CONTRIBUTING.md` — the Makefile targets, the conventions, and the traps that
  bite a test run. It is current.
- `docs/DESIGN.md` — twenty-four decisions, each with the alternative it beat. A
  question shaped "why does this not…" is almost always answered there.

`make all` (build, vet, staticcheck, race tests) has to pass before a change is
finished. It needs Docker or OrbStack running. Work goes straight to `main`:
eighteen commits, no branches, no merges.

## What the repo does not say about itself

**`.release/` and `docs/superpowers/` are excluded in `.git/info/exclude`, not
`.gitignore`.** That is deliberate — `git archive` replaces the tree's
`.gitignore` at every milestone replay, and these two rules have to survive it.
An agent that reads `.gitignore`, finds them missing and puts them in has undone
the reason. Neither directory is public; leave both alone.

**The three public documents avoid rhetorical negation.** `README.md`,
`docs/DESIGN.md` and `CONTRIBUTING.md` say what a thing does and stop. A clause
that defines the thing against an alternative nobody raised comes out: "X, not
Y", "not just X", a sentence excusing a code block for being there. A negation
that *is* the fact stays — something that does not happen, a value that is
refused, a rule written as a prohibition. Test each one by asking whether
anybody claimed the opposite. `docs/DESIGN.md` is the standing exception: its
declared shape is what it does, the obvious alternative, and why that
alternative is wrong, so its negations name alternatives the document raised
itself.

**The README's header is hand-written HTML inside the markdown** — a centered
`div` carrying the mark, the `h1`, the one-line thesis, two paragraphs and three
badges. That shape was set against the sibling repos and it stays put. After
editing `README.md`, check GitHub still renders the block:

    jq -Rs '{text: ., mode: "markdown"}' README.md > /tmp/md.json
    gh api -X POST /markdown --input /tmp/md.json | head -20

**The demo targets seed over HTTP.** All four — `make demo`, `make demo-post`,
`make demo-idempotency`, `make demo-serve` — open their chart through
`POST /v1/accounts` and post through `POST /v1/transactions`. Nothing under
`tools/` touches psql. All four ran green from a cold stack on 7 September 2026.

**A demo entry meant to be refused needs its own idempotency key.** Send the
refusal under a key an accepted entry already holds and the answer is 409, the
key conflict, rather than the 422 the step set out to show.
