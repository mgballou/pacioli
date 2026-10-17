package ledger_test

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"math/rand/v2"
	"sync"
	"testing"

	"github.com/mgballou/pacioli/internal/ledger"
	"github.com/mgballou/pacioli/internal/testdb"
)

const (
	propertyRunSeed       = uint64(0x9e3779b97f4a7c15)
	propertySequentialN   = 36
	propertyWriters       = 4
	propertyEntriesPerRun = 24
)

var propertyChart = map[string][]string{
	"EUR": {"assets.cash_eur", "liabilities.client_eur", "revenue.fees_eur", "expenses.ops_eur"},
	"GBP": {"assets.cash_gbp", "liabilities.client_gbp", "revenue.fees_gbp", "expenses.ops_gbp"},
	"USD": {"assets.cash_usd", "liabilities.client_usd", "revenue.fees_usd", "expenses.ops_usd"},
}

func TestRandomBalancedEntriesAndRefusalsStayBalancedWithConcurrentWriters(t *testing.T) {
	ctx := context.Background()
	db := testdb.Disposable(t, "ledger_property_test")
	seedPropertyChart(t, db)

	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		t.Fatalf("begin sequential batch (seed %d): %v", propertyRunSeed, err)
	}
	defer tx.Rollback()
	if err := runPropertyEntries(ctx, tx, propertyRunSeed, -1, propertySequentialN); err != nil {
		t.Fatalf("sequential batch (seed %d): %v", propertyRunSeed, err)
	}
	if err := checkPropertyTrial(ctx, tx, propertyRunSeed, "sequential batch"); err != nil {
		t.Fatal(err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatalf("commit sequential batch (seed %d): %v", propertyRunSeed, err)
	}
	assertPropertyTrial(t, db, "after sequential batch")

	start := make(chan struct{})
	results := make([]error, propertyWriters)
	var wg sync.WaitGroup
	for writer := range propertyWriters {
		wg.Add(1)
		go func(writer int) {
			defer wg.Done()
			<-start
			writerSeed := propertyRunSeed + uint64(writer+1)
			writerTx, err := db.BeginTx(ctx, nil)
			if err != nil {
				results[writer] = fmt.Errorf("begin writer %d (seed %d): %w", writer, writerSeed, err)
				return
			}
			if err := runPropertyEntries(ctx, writerTx, writerSeed, writer, propertyEntriesPerRun); err != nil {
				_ = writerTx.Rollback()
				results[writer] = err
				return
			}
			if err := checkPropertyTrial(ctx, writerTx, writerSeed, fmt.Sprintf("writer %d before commit", writer)); err != nil {
				_ = writerTx.Rollback()
				results[writer] = err
				return
			}
			if err := writerTx.Commit(); err != nil {
				results[writer] = fmt.Errorf("commit writer %d (seed %d): %w", writer, writerSeed, err)
			}
		}(writer)
	}
	close(start)
	wg.Wait()
	for writer, err := range results {
		if err != nil {
			t.Error(err)
		}
		_ = writer
	}
	assertPropertyTrial(t, db, "after concurrent batch")
	t.Logf("seed %d, %d sequential entries, %d writers x %d entries; all refusals left no rows",
		propertyRunSeed, propertySequentialN, propertyWriters, propertyEntriesPerRun)
}

func runPropertyEntries(ctx context.Context, tx *sql.Tx, seed uint64, writer, count int) error {
	rng := rand.New(rand.NewPCG(seed, seed^0xd1b54a32d192ed03))
	for entryIndex := range count {
		entry := generatedBalancedEntry(rng, seed, writer, entryIndex)
		if entryIndex%3 == 2 {
			refused := entry
			refused.Legs = append([]ledger.Leg(nil), entry.Legs...)
			refused.Description += " refused"
			refused.Legs[len(refused.Legs)-1].AmountMinor++
			if _, err := ledger.Post(ctx, tx, refused); !errors.Is(err, ledger.ErrUnbalanced) {
				return fmt.Errorf("seed %d writer %d entry %d refused post = %v, want %v; entry %+v",
					seed, writer, entryIndex, err, ledger.ErrUnbalanced, refused)
			}
			if err := assertNoPropertyRows(ctx, tx, refused.Description); err != nil {
				return fmt.Errorf("seed %d writer %d entry %d refusal left a trace: %w; entry %+v",
					seed, writer, entryIndex, err, refused)
			}
		}
		if _, err := ledger.Post(ctx, tx, entry); err != nil {
			return fmt.Errorf("seed %d writer %d entry %d balanced post: %w; entry %+v",
				seed, writer, entryIndex, err, entry)
		}
		if err := checkPropertyTrial(ctx, tx, seed, fmt.Sprintf("writer %d entry %d", writer, entryIndex)); err != nil {
			return err
		}
	}
	return nil
}

func generatedBalancedEntry(rng *rand.Rand, seed uint64, writer, index int) ledger.Entry {
	currencies := []string{"EUR", "GBP", "USD"}
	currency := currencies[rng.IntN(len(currencies))]
	accounts := propertyChart[currency]
	for {
		legsN := 2 + rng.IntN(len(accounts)-1)
		entry := ledger.Entry{
			Currency:    currency,
			Description: fmt.Sprintf("property seed %d writer %d entry %d", seed, writer, index),
		}
		var net int64
		for range legsN - 1 {
			amount := int64(2 + rng.IntN(1_000_000))
			if rng.IntN(2) == 0 {
				amount = -amount
			}
			net += amount
			entry.Legs = append(entry.Legs, ledger.Leg{Account: accounts[rng.IntN(len(accounts))], AmountMinor: amount})
		}
		if net < -1 || net > 1 {
			entry.Legs = append(entry.Legs, ledger.Leg{Account: accounts[rng.IntN(len(accounts))], AmountMinor: -net})
			return entry
		}
	}
}

func seedPropertyChart(t *testing.T, db *sql.DB) {
	t.Helper()
	for currency, accounts := range propertyChart {
		for index, code := range accounts {
			kind := []string{"asset", "liability", "revenue", "expense"}[index]
			if _, err := db.Exec(
				`INSERT INTO accounts (code, name, kind, currency) VALUES ($1, $2, $3, $4)`,
				code, code, kind, currency,
			); err != nil {
				t.Fatalf("seed property account %s: %v", code, err)
			}
		}
	}
}

func assertNoPropertyRows(ctx context.Context, tx *sql.Tx, description string) error {
	var transactions, postings int
	if err := tx.QueryRowContext(ctx,
		`SELECT count(*) FROM transactions WHERE description = $1`, description).Scan(&transactions); err != nil {
		return fmt.Errorf("count transaction rows: %w", err)
	}
	if err := tx.QueryRowContext(ctx,
		`SELECT count(*) FROM postings p JOIN transactions tr ON tr.id = p.transaction_id WHERE tr.description = $1`, description,
	).Scan(&postings); err != nil {
		return fmt.Errorf("count posting rows: %w", err)
	}
	if transactions != 0 || postings != 0 {
		return fmt.Errorf("found %d transaction rows and %d posting rows", transactions, postings)
	}
	return nil
}

func checkPropertyTrial(ctx context.Context, tx *sql.Tx, seed uint64, phase string) error {
	trials, err := ledger.TrialBalance(ctx, tx)
	if err != nil {
		return fmt.Errorf("seed %d %s trial balance: %w", seed, phase, err)
	}
	if len(trials) != len(propertyChart) {
		return fmt.Errorf("seed %d %s returned %d currency rows, want %d: %+v", seed, phase, len(trials), len(propertyChart), trials)
	}
	seen := make(map[string]bool, len(trials))
	for _, trial := range trials {
		seen[trial.Currency] = true
		if !trial.Balanced() || !trial.DebitsMinor.Equal(trial.CreditsMinor) {
			return fmt.Errorf("seed %d %s currency %s is not balanced: %+v", seed, phase, trial.Currency, trial)
		}
	}
	for currency := range propertyChart {
		if !seen[currency] {
			return fmt.Errorf("seed %d %s omitted currency %s from trial balance: %+v", seed, phase, currency, trials)
		}
	}
	return nil
}

func assertPropertyTrial(t *testing.T, db *sql.DB, phase string) {
	t.Helper()
	tx, err := db.BeginTx(context.Background(), &sql.TxOptions{ReadOnly: true})
	if err != nil {
		t.Fatalf("begin %s check: %v", phase, err)
	}
	defer tx.Rollback()
	if err := checkPropertyTrial(context.Background(), tx, propertyRunSeed, phase); err != nil {
		t.Error(err)
	}
}
