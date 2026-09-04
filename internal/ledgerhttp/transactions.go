package ledgerhttp

import (
	"crypto/sha256"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"regexp"
	"time"

	"github.com/mgballou/pacioli/internal/ledger"
)

// maxRequestBody bounds what will be read from a client body.
const maxRequestBody = 1 << 20

// The headers the idempotency contract is carried on.
const (
	keyHeader      = "Idempotency-Key"
	replayedHeader = "Idempotent-Replayed"
)

// A postingBody is one leg, in a request or a response.
type postingBody struct {
	Account     string `json:"account"`
	AmountMinor int64  `json:"amount_minor"`
}

// transactionRequest is the whole of what this endpoint accepts. The set is
// closed: DisallowUnknownFields below refuses anything else.
type transactionRequest struct {
	Currency    string        `json:"currency"`
	Description string        `json:"description"`
	Postings    []postingBody `json:"postings"`

	// OccurredAt is when the money moved. Absent means now.
	OccurredAt time.Time `json:"occurred_at"`
}

// net returns the sum of the legs and how many there were, so an unbalanced
// refusal can say how far out the entry was.
func (t transactionRequest) net() (int64, int) {
	var sum int64
	for _, p := range t.Postings {
		sum += p.AmountMinor
	}
	return sum, len(t.Postings)
}

// transactionBody is a transaction the ledger took: its id and its entry.
type transactionBody struct {
	Transaction string        `json:"transaction"`
	Currency    string        `json:"currency"`
	Description string        `json:"description"`
	Postings    []postingBody `json:"postings"`
}

// unbalancedBody says how far out an entry was and over how many legs.
type unbalancedBody struct {
	Error    string `json:"error"`
	NetMinor int64  `json:"net_minor"`
	Postings int    `json:"postings"`
}

func (s *server) postTransaction(w http.ResponseWriter, r *http.Request) {
	// Checked before the body: a write that cannot be made safe to retry is not read.
	key := r.Header.Get(keyHeader)
	if key == "" {
		s.write(w, r, http.StatusBadRequest, errorBody{
			Error:     "this endpoint will not take a write it cannot make safe to retry",
			Parameter: keyHeader,
		})
		return
	}

	r.Body = http.MaxBytesReader(w, r.Body, maxRequestBody)

	dec := json.NewDecoder(r.Body)
	dec.DisallowUnknownFields()

	var req transactionRequest
	if err := dec.Decode(&req); err != nil {
		s.unreadable(w, r, err)
		return
	}

	// The decoder stops at the first json value, so without this `{...}{...}`
	// would post the first entry and say nothing about the second.
	if err := dec.Decode(new(json.RawMessage)); !errors.Is(err, io.EOF) {
		s.write(w, r, http.StatusBadRequest, errorBody{
			Error: "the request body carries more than one json value",
		})
		return
	}

	e := ledger.Entry{
		Currency:    req.Currency,
		Description: req.Description,
		OccurredAt:  req.OccurredAt,
		Legs:        make([]ledger.Leg, 0, len(req.Postings)),
	}
	for _, p := range req.Postings {
		e.Legs = append(e.Legs, ledger.Leg{Account: p.Account, AmountMinor: p.AmountMinor})
	}

	claim, err := claimOf(key, req)
	if err != nil {
		s.logf("POST %s: %v", r.URL.RequestURI(), err)
		s.write(w, r, http.StatusInternalServerError, errorBody{Error: "internal error"})
		return
	}

	// One transaction holds the reservation, the post and the read-back. A replay
	// opens one too, because which it is only becomes known at the reservation.
	var (
		rec    ledger.Record
		stored ledger.Entry
	)
	err = s.ledger.Write(r.Context(), func(tx *sql.Tx) error {
		var err error
		rec, err = ledger.Once(r.Context(), tx, claim, func() (string, error) {
			return ledger.Post(r.Context(), tx, e)
		})
		if err != nil || !rec.Replayed {
			return err
		}
		stored, err = ledger.EntryOf(r.Context(), tx, rec.Transaction)
		return err
	})
	if err != nil {
		s.refuse(w, r, err, req, key)
		return
	}

	// A repeat gets the first answer, as the same 201, read back out of the ledger.
	if rec.Replayed {
		w.Header().Set(replayedHeader, "true")
		s.write(w, r, http.StatusCreated, entryBody(rec.Transaction, stored))
		return
	}

	// No Location header: nothing serves GET /v1/transactions/{id} yet.
	s.write(w, r, http.StatusCreated, transactionBody{
		Transaction: rec.Transaction,
		Currency:    req.Currency,
		Description: req.Description,
		Postings:    req.Postings,
	})
}

// claimOf fingerprints a request, so a key reused with a different one can be
// told from a retry. The digest is over the decoded request re-encoded, so
// whitespace and field order do not count.
func claimOf(key string, req transactionRequest) (ledger.Claim, error) {
	canonical, err := json.Marshal(req)
	if err != nil {
		return ledger.Claim{}, fmt.Errorf("fingerprint the request: %w", err)
	}
	sum := sha256.Sum256(canonical)
	return ledger.Claim{Key: key, RequestHash: sum[:]}, nil
}

// entryBody builds the answer from what the ledger holds, legs in written order.
func entryBody(transaction string, e ledger.Entry) transactionBody {
	body := transactionBody{
		Transaction: transaction,
		Currency:    e.Currency,
		Description: e.Description,
		Postings:    make([]postingBody, 0, len(e.Legs)),
	}
	for _, leg := range e.Legs {
		body.Postings = append(body.Postings, postingBody{
			Account:     leg.Account,
			AmountMinor: leg.AmountMinor,
		})
	}
	return body
}

// unreadable answers a body that could not be read as a request. The ledger was
// never asked, so every answer here is a 400 except the one about size.
func (s *server) unreadable(w http.ResponseWriter, r *http.Request, err error) {
	var tooBig *http.MaxBytesError
	if errors.As(err, &tooBig) {
		s.write(w, r, http.StatusRequestEntityTooLarge, errorBody{
			Error: "the request body is larger than this endpoint accepts",
			Value: fmt.Sprintf("%d bytes", tooBig.Limit),
		})
		return
	}

	var wrongType *json.UnmarshalTypeError
	if errors.As(err, &wrongType) && wrongType.Field != "" {
		s.write(w, r, http.StatusBadRequest, errorBody{
			Error:     "the field is not the type this endpoint takes",
			Parameter: wrongType.Field,
			Value:     wrongType.Value,
		})
		return
	}

	if name := unknownField(err); name != "" {
		s.write(w, r, http.StatusBadRequest, errorBody{Error: "no such field", Parameter: name})
		return
	}

	s.write(w, r, http.StatusBadRequest, errorBody{Error: "the request body is not json this endpoint can read"})
}

// DisallowUnknownFields reports through errors.New, so the field name is only in
// the text. No match means a refusal that names no field, never a guessed one.
var unknownFieldMessage = regexp.MustCompile(`^json: unknown field "(.*)"$`)

func unknownField(err error) string {
	m := unknownFieldMessage.FindStringSubmatch(err.Error())
	if m == nil {
		return ""
	}
	return m[1]
}

// refuse turns the ledger's sentinel errors into a status. It switches on the
// sentinel, never on the text of what Postgres said.
func (s *server) refuse(w http.ResponseWriter, r *http.Request, err error, req transactionRequest, key string) {
	// Which leg, where the refusal named one.
	named := errorBody{}
	var leg *ledger.LegError
	if errors.As(err, &leg) {
		named.Parameter = fmt.Sprintf("postings[%d]", leg.Index)
		named.Code = leg.Account
	}

	switch {
	case errors.Is(err, ledger.ErrKeyReused):
		s.write(w, r, http.StatusConflict, errorBody{
			Error:     "the idempotency key was used for a different request",
			Parameter: keyHeader,
			Value:     key,
		})

	case errors.Is(err, ledger.ErrBadKey):
		s.write(w, r, http.StatusUnprocessableEntity, errorBody{
			Error:     "the ledger will not hold that idempotency key",
			Parameter: keyHeader,
			Value:     key,
		})

	case errors.Is(err, ledger.ErrUnbalanced):
		sum, legs := req.net()
		s.write(w, r, http.StatusUnprocessableEntity, unbalancedBody{
			Error:    "the transaction does not balance",
			NetMinor: sum,
			Postings: legs,
		})

	case errors.Is(err, ledger.ErrUnknownAccount):
		named.Error = "no such account"
		s.write(w, r, http.StatusUnprocessableEntity, named)

	case errors.Is(err, ledger.ErrCurrencyMismatch):
		named.Error = "the account does not hold the transaction's currency"
		named.Value = req.Currency
		s.write(w, r, http.StatusUnprocessableEntity, named)

	case errors.Is(err, ledger.ErrRejected):
		// Everything else the schema refuses. Naming the constraint would mean a
		// copy of the schema in Go, so the server's own words go to the log.
		s.logf("POST %s: %v", r.URL.RequestURI(), err)
		named.Error = "the ledger refused the transaction"
		s.write(w, r, http.StatusUnprocessableEntity, named)

	default:
		s.logf("POST %s: %v", r.URL.RequestURI(), err)
		s.write(w, r, http.StatusInternalServerError, errorBody{Error: "internal error"})
	}
}
