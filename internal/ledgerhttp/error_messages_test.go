package ledgerhttp_test

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"testing"
)

func assertErrorMessage(t *testing.T, body []byte, want string) {
	t.Helper()

	var got struct {
		Error string `json:"error"`
	}
	if err := json.Unmarshal(body, &got); err != nil {
		t.Fatalf("unmarshal %q: %v", body, err)
	}
	if got.Error != want {
		t.Errorf("error = %q, want %q", got.Error, want)
	}
}

func TestMalformedQueryRefusalNamesItsMessage(t *testing.T) {
	for _, rawQuery := range []string{"%zz=1", "%"} {
		t.Run(rawQuery, func(t *testing.T) {
			srv := serve(t, empty)
			req, err := http.NewRequest(http.MethodGet, srv.URL+"/v1/accounts", nil)
			if err != nil {
				t.Fatalf("request: %v", err)
			}
			req.URL.RawQuery = rawQuery

			res, err := http.DefaultClient.Do(req)
			if err != nil {
				t.Fatalf("get: %v", err)
			}
			body, err := readBody(res)
			if err != nil {
				t.Fatalf("read response: %v", err)
			}
			if res.StatusCode != http.StatusBadRequest {
				t.Fatalf("status %d, want 400: %s", res.StatusCode, body)
			}
			assertErrorMessage(t, body, "the query string could not be read")
		})
	}
}

func TestMissingJSONMediaTypeRefusalNamesItsMessage(t *testing.T) {
	for _, tc := range []struct {
		name string
		path string
		with func(*http.Request)
	}{
		{name: "account without content type", path: "/v1/accounts", with: func(r *http.Request) { r.Header["Content-Type"] = nil }},
		{name: "transaction with unsupported content type", path: "/v1/transactions", with: func(r *http.Request) { r.Header.Set("Content-Type", "text/plain") }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			srv := serve(t, empty)
			res, body := postWith(t, srv.URL+tc.path, `{}`, tc.with)
			if res.StatusCode != http.StatusUnsupportedMediaType {
				t.Fatalf("status %d, want 415: %s", res.StatusCode, body)
			}
			assertErrorMessage(t, body, "this endpoint takes only a json body, and the request does not declare one")
		})
	}
}

func TestOversizedBodyRefusalNamesItsMessage(t *testing.T) {
	for _, tc := range []struct {
		name string
		path string
		body string
	}{
		{name: "account", path: "/v1/accounts", body: fmt.Sprintf(`{"code":"%s"}`, strings.Repeat("x", maxAccountBody))},
		{name: "transaction", path: "/v1/transactions", body: strings.Repeat(" ", maxTransactionBody+1)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			srv := serve(t, empty)
			res, body := post(t, srv.URL+tc.path, tc.body)
			if res.StatusCode != http.StatusRequestEntityTooLarge {
				t.Fatalf("status %d, want 413: %s", res.StatusCode, body)
			}
			assertErrorMessage(t, body, "the request body is larger than this endpoint accepts")
		})
	}
}

func TestSeveralUnknownBodyFieldsRefusalNamesItsMessage(t *testing.T) {
	for _, tc := range []struct {
		name string
		path string
		body string
	}{
		{name: "account", path: "/v1/accounts", body: `{"code":"cash","name":"Cash","kind":"asset","currency":"GBP","extra_a":1,"extra_b":2}`},
		{name: "transaction", path: "/v1/transactions", body: `{"currency":"GBP","description":"Refund","postings":[],"extra_a":1,"extra_b":2}`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			srv := serve(t, empty)
			res, body := post(t, srv.URL+tc.path, tc.body)
			if res.StatusCode != http.StatusBadRequest {
				t.Fatalf("status %d, want 400: %s", res.StatusCode, body)
			}
			assertErrorMessage(t, body, "the request body carries fields this endpoint cannot read")
		})
	}
}

func TestTrailingJSONValueRefusalNamesItsMessage(t *testing.T) {
	for _, tc := range []struct {
		name string
		path string
		body string
	}{
		{name: "account", path: "/v1/accounts", body: `{"code":"cash"}{}`},
		{name: "transaction", path: "/v1/transactions", body: `{"currency":"GBP"}{}`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			srv := serve(t, empty)
			res, body := post(t, srv.URL+tc.path, tc.body)
			if res.StatusCode != http.StatusBadRequest {
				t.Fatalf("status %d, want 400: %s", res.StatusCode, body)
			}
			assertErrorMessage(t, body, "the request body carries more than one json value")
		})
	}
}

func readBody(res *http.Response) ([]byte, error) {
	defer res.Body.Close()
	return io.ReadAll(res.Body)
}
