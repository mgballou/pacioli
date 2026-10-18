package ledger

import (
	"encoding/json"
	"testing"
)

func FuzzMinorJSONRoundTrip(f *testing.F) {
	for _, amount := range []string{"0", "1", "-1", "9223372036854775807", "-9223372036854775808", "9223372036854775808", "-9223372036854775809", "1234567890123456789012345678901234567890"} {
		f.Add(amount)
	}
	f.Fuzz(func(t *testing.T, input string) {
		var before Minor
		if err := json.Unmarshal([]byte(input), &before); err != nil {
			return
		}

		encoded, err := json.Marshal(before)
		if err != nil {
			t.Fatalf("marshal accepted amount %q: %v", input, err)
		}
		var after Minor
		if err := json.Unmarshal(encoded, &after); err != nil {
			t.Fatalf("unmarshal round-tripped amount %q: %v", encoded, err)
		}
		if !before.Equal(after) {
			t.Fatalf("amount changed from %s to %s", before, after)
		}
	})
}
