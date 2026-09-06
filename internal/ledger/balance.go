package ledger

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"slices"
	"strings"
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

// ErrUnknownKind means a filter, or an account being opened, named something
// that is not an account_kind. The kinds are a closed set, so every refusal
// carrying this sentinel is an *UnknownKindError and lists them.
var ErrUnknownKind = errors.New("no such account kind")

// An UnknownKindError names the kind that was given and the kinds the schema
// actually holds. It is typed so a caller can hand the set on without reading
// this package's error text, the way LegError carries the leg.
type UnknownKindError struct {
	Kind  string   // what was given
	Valid []string // the kinds account_kind holds, in the order the schema declares them
	Err   error    // the server's own refusal, where the server is what refused it
}

func (e *UnknownKindError) Error() string {
	msg := fmt.Sprintf("%s: %q", ErrUnknownKind, shown(e.Kind))
	if len(e.Valid) > 0 {
		msg += ", which is not one of " + strings.Join(e.Valid, ", ")
	}
	if e.Err != nil {
		msg += ": " + e.Err.Error()
	}
	return msg
}

// Unwrap keeps errors.Is finding the sentinel and errors.As the server's own.
func (e *UnknownKindError) Unwrap() []error {
	if e.Err == nil {
		return []error{ErrUnknownKind}
	}
	return []error{ErrUnknownKind, e.Err}
}

// An AccountFilter narrows a list of accounts. Its zero value is every account:
// an empty field means the caller did not ask, and no account holds a blank one.
type AccountFilter struct {
	// Currency is matched exactly and checked against nothing. The set is
	// open, so a code no account holds is an empty answer, not a refusal.
	Currency string

	// Kind is one of the account_kind values, a closed set, so a value
	// outside it is ErrUnknownKind. The enum in Postgres is what checks.
	Kind string
}

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
		// No valid set to offer: account codes are open, and the chart is
		// what says which exist.
		return Balance{}, fmt.Errorf("%w: %q", ErrUnknownAccount, shown(code))
	case err != nil:
		return Balance{}, fmt.Errorf("read the balance of %q: %w", shown(code), err)
	}
	return b, nil
}

// Balances returns the accounts f matches, in code order, including the ones
// nothing has been posted to. Every filter is always bound and an unasked one
// reads as NULL, so the statement is fixed rather than assembled from strings.
func Balances(ctx context.Context, tx *sql.Tx, f AccountFilter) ([]Balance, error) {
	if err := checkKind(ctx, tx, f.Kind); err != nil {
		return nil, err
	}

	rows, err := tx.QueryContext(ctx,
		`SELECT `+balanceColumns+`
		   FROM account_balances
		  WHERE (nullif($1::text, '') IS NULL OR currency = $1::text)
		    AND (nullif($2::text, '') IS NULL OR kind::text = $2::text)
		  ORDER BY code`, f.Currency, f.Kind)
	if err != nil {
		return nil, fmt.Errorf("list balances (currency %q, kind %q): %w", shown(f.Currency), shown(f.Kind), err)
	}
	defer rows.Close()

	var out []Balance
	for rows.Next() {
		var b Balance
		if err := rows.Scan(&b.Account, &b.Name, &b.Kind, &b.Currency, &b.AmountMinor, &b.Postings); err != nil {
			return nil, fmt.Errorf("read a balance (currency %q, kind %q): %w", shown(f.Currency), shown(f.Kind), err)
		}
		out = append(out, b)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("list balances (currency %q, kind %q): %w", shown(f.Currency), shown(f.Kind), err)
	}
	return out, nil
}

// checkKind asks before it filters, because casting a bad kind would raise and
// leave the caller's transaction aborted for every read after it. It reads the
// whole enum rather than asking whether one value is in it, so the refusal can
// list the kinds there are.
func checkKind(ctx context.Context, tx *sql.Tx, kind string) error {
	if kind == "" {
		return nil
	}
	kinds, err := accountKinds(ctx, tx)
	if err != nil {
		return fmt.Errorf("read the account kinds to check %q against: %w", shown(kind), err)
	}
	if slices.Contains(kinds, kind) {
		return nil
	}
	return &UnknownKindError{Kind: kind, Valid: kinds}
}

// accountKinds reads the enum out of the schema, so a kind added there is
// accepted, and listed in a refusal, with no edit here.
func accountKinds(ctx context.Context, tx *sql.Tx) ([]string, error) {
	rows, err := tx.QueryContext(ctx, `SELECT unnest(enum_range(NULL::account_kind))::text`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var kinds []string
	for rows.Next() {
		var kind string
		if err := rows.Scan(&kind); err != nil {
			return nil, err
		}
		kinds = append(kinds, kind)
	}
	return kinds, rows.Err()
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
		return nil, fmt.Errorf("read the trial balance: %w", err)
	}
	defer rows.Close()

	var out []Trial
	for rows.Next() {
		var t Trial
		if err := rows.Scan(&t.Currency, &t.DebitsMinor, &t.CreditsMinor, &t.NetMinor, &t.Accounts, &t.Postings); err != nil {
			return nil, fmt.Errorf("read one currency's totals: %w", err)
		}
		out = append(out, t)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("read the trial balance: %w", err)
	}
	return out, nil
}
