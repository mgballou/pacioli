# Check state

Measured 20 September 2026 on the `zai-pacioli-suite-green` worktree, branch
`feat/suite-green` at `9f86fc7`, identical to local `main`. Toolchain: go
1.26.7 darwin/arm64, Docker server 29.4.0. Test database: Postgres from
`compose.test.yaml` on 127.0.0.1:55432.

Each target ran alone, in order, with its output captured whole. Nothing was
red, so nothing was fixed and no test was changed.

## Targets

| Target | Exit | Wall | What it said |
|---|---|---|---|
| `make build` | 0 | 1 s | Compiled `bin/pacioli`, version stamp `9f86fc7`. |
| `make vet` | 0 | 1 s | No findings. |
| `make lint` | 0 | 4 s | staticcheck 2025.1.1, no findings. |
| `make db-up` | 0 | 1 s | Container `pacioli-test-db` reported healthy. |
| `make test` | 0 | 4 s | Five packages ok, one with no test files. |
| `make fmt` | 0 | under 1 s | `gofmt -l` listed nothing. |
| `make all` | 0 | 2 s | build, vet, lint, test; tests served from cache. |
| `make negative-controls` | 0 | 130 s | All 27 controls green before their mutation and red after. |
| `make db-down` | 0 | 1 s | Container and network removed; nothing left running. |

`make fmt` and `make negative-controls` are not part of `make all`; CI runs
both, so they were measured too.

## Numbers

- Packages under `./...`: 6. Five carry tests; `tools/controls` has none.
- Test files outside `vendor/`: 16.
- Tests: 110 top-level and 21 subtests pass. 0 fail. 0 skip.
- Source of the counts: `go test -race -count=1 -v ./...`, exit 0, 4 s wall.
- Uncached package times against a fresh schema: `internal/ledger` 2.800 s,
  `internal/schema` 1.344 s, `internal/ledgerhttp` 2.747 s, `internal/testdb`
  1.963 s, `cmd/pacioli` 3.103 s.
- Negative controls: 27 declared, 27 green before and red after, 0 already red,
  0 still green after.

Two checks that the green means what it says:

1. The suite needs a live database. After `make db-down`,
   `go test -count=1 ./internal/ledger/` fails every test at connect, refused
   on 127.0.0.1:55432. No test skips when the database is missing.
2. A warm container can hold a stale schema, and `schema.Apply` is a no-op
   against one (see CONTRIBUTING). The counts above come from a `-count=1` run
   against a database the controls pass had just reset.

## Failing packages

None. Every target exited 0 and every test passed, so there was no failure to
fix and none to leave red.

One operational note, not a failure: `make negative-controls` refuses to run on
any untracked path, and this worktree carries `.fastembed_cache/`, dropped by a
tool outside this repository. That directory was moved aside for the run and
restored after. Until it is removed or ignored, the controls target will
refuse here.

## What it takes for `make all` to pass

It passes today. Ordered by the work each item would take:

1. Nothing. `make all` exits 0 as measured above.
2. Docker running, for `make test` to have its database. It was up.
3. A clean checkout for `make negative-controls`, which CI runs as a second
   job. The foreign `.fastembed_cache/` directory currently blocks it here.
4. Reconciling local `main` with `origin/main` — 9 ahead, 37 behind. That is
   repository housekeeping, not a check, and it does not affect any target.
