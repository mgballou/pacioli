package ledgerhttp

import (
	"database/sql"
	"errors"
	"net/http"

	"github.com/mgballou/pacioli/internal/docs"
	"github.com/mgballou/pacioli/internal/ledger"
)

// accountRequest is the whole of what this endpoint accepts. The set is closed.
type accountRequest struct {
	Code     string `json:"code"`
	Name     string `json:"name"`
	Kind     string `json:"kind"`
	Currency string `json:"currency"`
}

func (s *server) openAccount(w http.ResponseWriter, r *http.Request) {
	// Checked before the body, because it is what decides whether the body
	// should have been sent at all.
	if !s.declaredJSON(w, r) {
		return
	}

	var req accountRequest
	if !s.decode(w, r, &req) {
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

// value hands back what this request carried under a json field name.
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

// refuseAccount turns the ledger's sentinel errors into a status.
func (s *server) refuseAccount(w http.ResponseWriter, r *http.Request, err error, req accountRequest) {
	switch {
	case cancelled(err):
		s.gaveUp(w, r)

	case errors.Is(err, ledger.ErrAccountExists):
		// Which account holds it stays out of the body; See points at what
		// serves that.
		s.write(w, r, http.StatusConflict, errorBody{
			Error:     "an account already holds that code",
			Parameter: "code",
			Value:     shown(req.Code),
			Expected:  "a code no account holds",
			See:       "GET /v1/accounts/" + shown(req.Code) + " says what holds it; " + docs.Home,
		})

	case errors.Is(err, ledger.ErrUnknownKind):
		s.write(w, r, http.StatusUnprocessableEntity, errorBody{
			Error:     "no such account kind",
			Parameter: "kind",
			Value:     shown(req.Kind),
			Valid:     validKinds(err),
			See:       docs.Home,
		})

	case errors.Is(err, ledger.ErrRejected):
		// A bad code, a blank name, a currency that is not three capitals.
		s.logf("POST %s: %v", r.URL.RequestURI(), err)
		body := errorBody{
			Error: "the ledger will not hold that account",
			Code:  shown(req.Code),
			See:   docs.Home,
		}
		if field := refusedField(err, accountFields); field != "" {
			value := req.value(field)
			body.Parameter = field
			body.Value = shown(value)
			bound(&body, field, value)
		}
		s.write(w, r, http.StatusUnprocessableEntity, body)

	default:
		s.logf("POST %s: %v", r.URL.RequestURI(), err)
		s.write(w, r, http.StatusInternalServerError, errorBody{Error: "internal error"})
	}
}
