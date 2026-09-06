package ledgerhttp

import (
	"database/sql"
	"encoding/json"
	"errors"
	"io"
	"net/http"

	"github.com/mgballou/pacioli/internal/docs"
	"github.com/mgballou/pacioli/internal/ledger"
)

// accountRequest is the whole of what this endpoint accepts. The set is closed,
// so a misspelt field is refused by name rather than dropped.
type accountRequest struct {
	Code     string `json:"code"`
	Name     string `json:"name"`
	Kind     string `json:"kind"`
	Currency string `json:"currency"`
}

func (s *server) openAccount(w http.ResponseWriter, r *http.Request) {
	r.Body = http.MaxBytesReader(w, r.Body, maxRequestBody)

	dec := json.NewDecoder(r.Body)
	dec.DisallowUnknownFields()

	var req accountRequest
	if err := dec.Decode(&req); err != nil {
		s.unreadable(w, r, err, accountFields)
		return
	}
	// The decoder stops at the first json value, so without this `{...}{...}`
	// would open the first account and say nothing about the second.
	if err := dec.Decode(new(json.RawMessage)); !errors.Is(err, io.EOF) {
		s.write(w, r, http.StatusBadRequest, errorBody{
			Error:    "the request body carries more than one json value",
			Expected: "one json object, and nothing after it",
			See:      docs.Home,
		})
		return
	}

	// One transaction holds the open and the read-back that answers it.
	var b ledger.Balance
	err := s.ledger.Write(r.Context(), func(tx *sql.Tx) error {
		if err := ledger.Open(r.Context(), tx, ledger.Account{
			Code:     req.Code,
			Name:     req.Name,
			Kind:     req.Kind,
			Currency: req.Currency,
		}); err != nil {
			return err
		}
		var err error
		b, err = ledger.BalanceOf(r.Context(), tx, req.Code)
		return err
	})
	if err != nil {
		s.refuseAccount(w, r, err, req)
		return
	}

	// A Location here, unlike on a transaction: GET /v1/accounts/{code} serves it.
	w.Header().Set("Location", "/v1/accounts/"+req.Code)
	s.write(w, r, http.StatusCreated, balanceOf(b))
}

// value hands back what this request carried under a json field name, so a
// refusal that names a field can hand the client its own value for it.
func (req accountRequest) value(field string) string {
	switch field {
	case "code":
		return req.Code
	case "name":
		return req.Name
	case "kind":
		return req.Kind
	case "currency":
		return req.Currency
	}
	return ""
}

// refuseAccount turns the ledger's sentinel errors into a status, as fail and
// refuse do.
func (s *server) refuseAccount(w http.ResponseWriter, r *http.Request, err error, req accountRequest) {
	switch {
	case errors.Is(err, ledger.ErrAccountExists):
		// Which account holds it is not put in the body: GET /v1/accounts/{code}
		// serves that already, and pointing there says it without repeating it.
		s.write(w, r, http.StatusConflict, errorBody{
			Error:     "an account already holds that code",
			Parameter: "code",
			Value:     shown(req.Code),
			Expected:  "a code no account holds",
			See:       "GET /v1/accounts/" + req.Code + " says what holds it; " + docs.Home,
		})

	case errors.Is(err, ledger.ErrUnknownKind):
		// 422 rather than 400: the request was read, and the enum in the
		// schema is what refused it.
		s.write(w, r, http.StatusUnprocessableEntity, errorBody{
			Error:     "no such account kind",
			Parameter: "kind",
			Value:     shown(req.Kind),
			Valid:     validKinds(err),
			See:       docs.Home,
		})

	case errors.Is(err, ledger.ErrRejected):
		// A bad code, a blank name, a currency that is not three capitals.
		// Naming the constraint would mean a copy of the schema in Go, so the
		// server's own words go to the log; the field it named, and the value
		// the client sent for that field, come back.
		s.logf("POST %s: %v", r.URL.RequestURI(), err)
		body := errorBody{
			Error: "the ledger will not hold that account",
			Code:  req.Code,
			See:   docs.Home,
		}
		if field := refusedField(err, accountFields); field != "" {
			body.Parameter = field
			body.Value = shown(req.value(field))
		}
		s.write(w, r, http.StatusUnprocessableEntity, body)

	default:
		s.logf("POST %s: %v", r.URL.RequestURI(), err)
		s.write(w, r, http.StatusInternalServerError, errorBody{Error: "internal error"})
	}
}
