package ledgerhttp

import (
	"net/http/httptest"
	"strings"
	"testing"
)

func FuzzAccountBody(f *testing.F) {
	f.Add("{\"code\":\"cash\",\"name\":\"Cash\",\"kind\":\"asset\",\"currency\":\"USD\"}")
	f.Add("{}")
	f.Add("[]")
	f.Add("{\"name\":7}")
	f.Add("{\"name\":\"Cash\"} {}")
	f.Fuzz(func(t *testing.T, body string) {
		var req accountRequest
		w := httptest.NewRecorder()
		r := httptest.NewRequest("POST", "/v1/accounts", strings.NewReader(body))
		if !((&server{}).decode(w, r, &req, maxAccountBody)) && w.Code != 400 && w.Code != 413 {
			t.Fatalf("body reader refused with HTTP %d, want 400 or 413", w.Code)
		}
	})
}

func FuzzTransactionBody(f *testing.F) {
	f.Add("{\"currency\":\"USD\",\"description\":\"opening\",\"postings\":[{\"account\":\"cash\",\"amount_minor\":1}]}")
	f.Add("{}")
	f.Add("null")
	f.Add("{\"postings\":[{\"amount_minor\":9223372036854775808}]}")
	f.Add("{\"postings\":[]} {}")
	f.Fuzz(func(t *testing.T, body string) {
		var req transactionRequest
		w := httptest.NewRecorder()
		r := httptest.NewRequest("POST", "/v1/transactions", strings.NewReader(body))
		if !((&server{}).decode(w, r, &req, maxTransactionBody)) && w.Code != 400 && w.Code != 413 {
			t.Fatalf("body reader refused with HTTP %d, want 400 or 413", w.Code)
		}
	})
}
