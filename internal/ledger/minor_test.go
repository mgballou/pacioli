package ledger_test

import (
	"testing"

	"github.com/mgballou/pacioli/internal/ledger"
)

func TestMinorReadsWholeJSONAmounts(t *testing.T) {
	for _, tc := range []struct {
		name string
		json string
		want string
		bad  bool
	}{
		{name: "zero", json: "0", want: "0"},
		{name: "negative", json: "-42", want: "-42"},
		{name: "past int64", json: "9223372036854775808", want: "9223372036854775808"},
		{name: "fraction", json: "1.5", bad: true},
		{name: "text", json: `"12"`, bad: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var got ledger.Minor
			err := got.UnmarshalJSON([]byte(tc.json))
			if tc.bad {
				if err == nil {
					t.Fatal("UnmarshalJSON accepted a value that is not a whole number")
				}
				return
			}
			if err != nil {
				t.Fatalf("UnmarshalJSON: %v", err)
			}
			if got.String() != tc.want {
				t.Errorf("amount = %s, want %s", got, tc.want)
			}
		})
	}
}

func TestMinorScansDatabaseValuesWithoutFloats(t *testing.T) {
	for _, tc := range []struct {
		name string
		src  any
		want string
		bad  bool
	}{
		{name: "null", src: nil, want: "0"},
		{name: "integer", src: int64(-17), want: "-17"},
		{name: "numeric string", src: "123456789012345678901", want: "123456789012345678901"},
		{name: "numeric bytes", src: []byte("-90000000000000000000"), want: "-90000000000000000000"},
		{name: "fractional string", src: "1.5", bad: true},
		{name: "float", src: float64(3), bad: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var got ledger.Minor
			err := got.Scan(tc.src)
			if tc.bad {
				if err == nil {
					t.Fatal("Scan accepted a value outside its database types")
				}
				return
			}
			if err != nil {
				t.Fatalf("Scan: %v", err)
			}
			if got.String() != tc.want {
				t.Errorf("amount = %s, want %s", got, tc.want)
			}
		})
	}
}
