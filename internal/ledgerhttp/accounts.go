package ledgerhttp

import (
	"database/sql"
	"encoding/json"
	"errors"
	"io"
	"net/http"

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
		s.unreadable(w, r, err)
		return
	}
	// The decoder stops at the first json value, so without this `{...}{...}`
	// would open the first account and say nothing about the second.
	if err := dec.Decode(new(json.RawMessage)); !errors.Is(err, io.EOF) {
		s.write(w, r, http.StatusBadRequest, errorBody{
			Error: "the request body carries more than one json value",
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

// refuseAccount turns the ledger's sentinel errors into a status, as fail and
// refuse do.
func (s *server) refuseAccount(w http.ResponseWriter, r *http.Request, err error, req accountRequest) {
	switch {
	case errors.Is(err, ledger.ErrAccountExists):
		s.write(w, r, http.StatusConflict, errorBody{
			Error:     "an account already holds that code",
			Parameter: "code",
			Value:     req.Code,
		})

	case errors.Is(err, ledger.ErrUnknownKind):
		// 422 rather than 400: the request was read, and the enum in the
		// schema is what refused it.
		s.write(w, r, http.StatusUnprocessableEntity, errorBody{
			Error:     "no such account kind",
			Parameter: "kind",
			Value:     req.Kind,
		})

	case errors.Is(err, ledger.ErrRejected):
		// A bad code, a blank name, a currency that is not three capitals.
		// Naming the constraint would mean a copy of the schema in Go, so
		// the server's own words go to the log.
		s.logf("POST %s: %v", r.URL.RequestURI(), err)
		s.write(w, r, http.StatusUnprocessableEntity, errorBody{
			Error: "the ledger will not hold that account",
			Code:  req.Code,
		})

	default:
		s.logf("POST %s: %v", r.URL.RequestURI(), err)
		s.write(w, r, http.StatusInternalServerError, errorBody{Error: "internal error"})
	}
}
