package ledger

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
)

// A Balance is one account's position, derived from its postings. Signed the way
// a Leg is: debit positive, credit negative.
type Balance struct {
	Account     string // the account code, e.g. "assets.cash"
	Name        string
	Kind        string // one of the account_kind values: asset, liability, equity, revenue, expense
	Currency    string
	AmountMinor int64
	Postings    int64 // how many legs have landed on this account
}

// A Trial is one currency's side totals. Per currency and never across: minor
// units of different currencies do not add up.
type Trial struct {
	Currency     string
	DebitsMinor  int64 // sum of the positive legs
	CreditsMinor int64 // sum of the negative legs, as a positive number
	NetMinor     int64 // debits minus credits; zero, or the books are wrong
	Accounts     int64
	Postings     int64
}

// Balanced reports whether the two sides cancel.
func (t Trial) Balanced() bool { return t.NetMinor == 0 }

// balanceColumns reads the account_balances view, so there is one definition of
// an account balance.
const balanceColumns = `code, name, kind::text, currency, balance_minor, posting_count`

// BalanceOf returns one account's balance. A code no account holds is
// ErrUnknownAccount, never a zero balance.
func BalanceOf(ctx context.Context, tx *sql.Tx, code string) (Balance, error) {
	var b Balance
	err := tx.QueryRowContext(ctx,
		`SELECT `+balanceColumns+` FROM account_balances WHERE code = $1`, code,
	).Scan(&b.Account, &b.Name, &b.Kind, &b.Currency, &b.AmountMinor, &b.Postings)
	switch {
	case errors.Is(err, sql.ErrNoRows):
		return Balance{}, fmt.Errorf("%w %q", ErrUnknownAccount, code)
	case err != nil:
		return Balance{}, fmt.Errorf("balance of %q: %w", code, err)
	}
	return b, nil
}

// Balances returns every account, in code order, including the ones nothing has
// been posted to.
func Balances(ctx context.Context, tx *sql.Tx) ([]Balance, error) {
	rows, err := tx.QueryContext(ctx, `SELECT `+balanceColumns+` FROM account_balances ORDER BY code`)
	if err != nil {
		return nil, fmt.Errorf("balances: %w", err)
	}
	defer rows.Close()

	var out []Balance
	for rows.Next() {
		var b Balance
		if err := rows.Scan(&b.Account, &b.Name, &b.Kind, &b.Currency, &b.AmountMinor, &b.Postings); err != nil {
			return nil, fmt.Errorf("balances: %w", err)
		}
		out = append(out, b)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("balances: %w", err)
	}
	return out, nil
}

// TrialBalance returns the side totals for every currency the chart holds, in
// currency order. Currencies come from accounts rather than postings, so one set
// up and never used reports as zeroes instead of vanishing.
func TrialBalance(ctx context.Context, tx *sql.Tx) ([]Trial, error) {
	rows, err := tx.QueryContext(ctx,
		`SELECT a.currency,
		        coalesce(sum(p.amount_minor) FILTER (WHERE p.amount_minor > 0), 0)  AS debits,
		        coalesce(-sum(p.amount_minor) FILTER (WHERE p.amount_minor < 0), 0) AS credits,
		        coalesce(sum(p.amount_minor), 0)                                    AS net,
		        count(DISTINCT a.id)                                                AS accounts,
		        count(p.id)                                                         AS postings
		   FROM accounts a
		   LEFT JOIN postings p ON p.account_id = a.id
		  GROUP BY a.currency
		  ORDER BY a.currency`)
	if err != nil {
		return nil, fmt.Errorf("trial balance: %w", err)
	}
	defer rows.Close()

	var out []Trial
	for rows.Next() {
		var t Trial
		if err := rows.Scan(&t.Currency, &t.DebitsMinor, &t.CreditsMinor, &t.NetMinor, &t.Accounts, &t.Postings); err != nil {
			return nil, fmt.Errorf("trial balance: %w", err)
		}
		out = append(out, t)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("trial balance: %w", err)
	}
	return out, nil
}
