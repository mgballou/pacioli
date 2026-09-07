package schema_test

import (
	"testing"

	"github.com/mgballou/pacioli/internal/testdb"
)

// `btrim(x) <> ''` trims spaces and nothing else, so a name of one newline was a
// name and a description of one tab was a description. is_blank and
// has_control_character are what the schema asks now, and these say what they
// hold every field of a person's words to.
//
// A refusal aborts the transaction it was made in, so every one of these takes a
// transaction of its own.

// The control characters a keyboard produces, and the ones a terminal acts on.
var controls = map[string]string{
	"a line feed":       "\n",
	"a carriage return": "\r",
	"a tab":             "\t",
	"a vertical tab":    "\v",
	"a form feed":       "\f",
	"a backspace":       "\b",
	"a bell":            "\a",
	"an escape":         "\x1b",
	"a delete":          "\x7f",
}

func TestAValueOfNothingButOneControlCharacterIsRefused(t *testing.T) {
	for name, c := range controls {
		t.Run(name, func(t *testing.T) {
			assertCode(t, writeName(t, c), codeCheck)
			assertCode(t, writeDescription(t, c), codeCheck)
		})
	}
}

// Blank is about what is there, not where. A control character smuggled into the
// middle of a readable value is the one a report or a log line would carry.
func TestAControlCharacterInsideAValueIsRefused(t *testing.T) {
	for name, c := range controls {
		t.Run(name, func(t *testing.T) {
			assertCode(t, writeName(t, "Cash"+c+"at bank"), codeCheck)
			assertCode(t, writeDescription(t, "Customer"+c+"deposit"), codeCheck)
		})
	}
}

// The half of the old rule that was already right.
func TestAValueOfNothingButSpacesIsRefused(t *testing.T) {
	assertCode(t, writeName(t, "   "), codeCheck)
	assertCode(t, writeDescription(t, " "), codeCheck)
}

// A rule that refused everything would pass every test above.
func TestAValueAPersonCanReadIsHeld(t *testing.T) {
	if err := writeName(t, " Cash at bank "); err != nil {
		t.Errorf("a name with a word in it was refused: %v", err)
	}
	if err := writeDescription(t, "Customer deposit, less fee — 15%"); err != nil {
		t.Errorf("a description with words in it was refused: %v", err)
	}
}

// The code, the currency and the idempotency key are closed sets of characters
// rather than open text, so the rule above would say nothing they do not already
// say. This is what makes that claim checkable: Postgres anchors ^ and $ to the
// whole string, and a trailing newline is not a second line to them.
func TestTheClosedShapesAlreadyRefuseAControlCharacter(t *testing.T) {
	for name, c := range controls {
		t.Run(name, func(t *testing.T) {
			tx := testdb.Tx(t)
			_, err := tx.Exec(
				`INSERT INTO accounts (code, name, kind, currency) VALUES ($1, 'Cash', 'asset', 'GBP')`,
				"assets.cash"+c)
			assertCode(t, err, codeCheck)

			tx = testdb.Tx(t)
			_, err = tx.Exec(
				`INSERT INTO accounts (code, name, kind, currency) VALUES ('assets.other', 'Cash', 'asset', $1)`,
				"GBP"+c)
			assertCode(t, err, codeCheck)

			tx = testdb.Tx(t)
			_, err = tx.Exec(
				`INSERT INTO idempotency_keys (key, request_hash) VALUES ($1, sha256('body'))`,
				"a-key-long-enough"+c)
			assertCode(t, err, codeCheck)
		})
	}
}

func writeName(t *testing.T, name string) error {
	t.Helper()

	_, err := testdb.Tx(t).Exec(
		`INSERT INTO accounts (code, name, kind, currency) VALUES ('assets.blank', $1, 'asset', 'GBP')`, name)
	return err
}

func writeDescription(t *testing.T, description string) error {
	t.Helper()

	_, err := testdb.Tx(t).Exec(
		`INSERT INTO transactions (currency, description) VALUES ('GBP', $1)`, description)
	return err
}
