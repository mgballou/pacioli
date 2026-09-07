package ledger

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"math/big"
)

// A Minor is an amount in minor units, wide enough for any total the ledger can
// derive from its postings.
//
// One posting is `amount_minor bigint` and fits an int64. A sum of postings is
// not: Postgres answers `sum(bigint)` with `numeric`, which has no ceiling, and
// so has this. Every derived total — an account balance, a side of the trial
// balance, what an entry nets to — is a Minor, and every single posting stays an
// int64, because that is what each of the two is in the schema.
//
// A Minor is immutable. Its zero value is zero minor units, and copying one is
// safe because no method writes through the receiver.
type Minor struct{ n big.Int }

// MinorOf is the amount an int64 holds.
func MinorOf(v int64) Minor {
	var m Minor
	m.n.SetInt64(v)
	return m
}

// Add returns m + o.
func (m Minor) Add(o Minor) Minor {
	var sum Minor
	sum.n.Add(&m.n, &o.n)
	return sum
}

// Neg returns -m, so a credit read off a row can be reported as a positive
// total.
func (m Minor) Neg() Minor {
	var neg Minor
	neg.n.Neg(&m.n)
	return neg
}

// Sign is -1 for a credit, 0 for nothing, and +1 for a debit.
func (m Minor) Sign() int { return m.n.Sign() }

// IsZero reports whether m is exactly zero.
func (m Minor) IsZero() bool { return m.n.Sign() == 0 }

// Equal reports whether m and o are the same amount. Minor holds a slice, so it
// is not comparable with ==.
func (m Minor) Equal(o Minor) bool { return m.n.Cmp(&o.n) == 0 }

// String is the amount in decimal digits, with a leading minus for a credit.
func (m Minor) String() string { return m.n.String() }

// MarshalJSON writes the amount as a JSON number, which is what a fixed-width
// integer wrote before it and what `numeric` means.
func (m Minor) MarshalJSON() ([]byte, error) { return []byte(m.n.String()), nil }

// UnmarshalJSON reads a JSON number that is a whole number of minor units.
func (m *Minor) UnmarshalJSON(b []byte) error {
	if _, ok := m.n.SetString(string(b), 10); !ok {
		return fmt.Errorf("read %s as a whole number of minor units", shown(string(b)))
	}
	return nil
}

// Scan reads a `numeric` off a row. The pgx driver hands one over as its digits,
// so nothing here goes through a fixed-width type or a float on the way.
func (m *Minor) Scan(src any) error {
	var digits string
	switch v := src.(type) {
	case nil:
		m.n.SetInt64(0)
		return nil
	case int64:
		m.n.SetInt64(v)
		return nil
	case string:
		digits = v
	case []byte:
		digits = string(v)
	default:
		return fmt.Errorf("read %T as minor units", src)
	}
	if _, ok := m.n.SetString(digits, 10); !ok {
		return fmt.Errorf("read %q as a whole number of minor units", shown(digits))
	}
	return nil
}

// The interfaces a Minor has to satisfy to cross the two boundaries it crosses:
// a row on the way in, a response body on the way out. Nothing calls these by
// name, so without this nothing would break a compile if one were dropped.
var (
	_ sql.Scanner      = (*Minor)(nil)
	_ json.Marshaler   = Minor{}
	_ json.Unmarshaler = (*Minor)(nil)
)
