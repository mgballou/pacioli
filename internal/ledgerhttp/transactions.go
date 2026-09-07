package ledgerhttp

import (
	"crypto/sha256"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"maps"
	"net/http"
	"reflect"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/jackc/pgx/v5/pgconn"
	"github.com/mgballou/pacioli/internal/docs"
	"github.com/mgballou/pacioli/internal/ledger"
)

// maxRequestBody bounds what will be read from a client body.
const maxRequestBody = 1 << 20

// maxPostings bounds the legs one entry may carry.
//
// The 1 MB body is a backstop, not a bound: it let one entry carry 20,900 legs
// and take 21 seconds. A thousand is past anything double entry produces — most
// entries have two, a consolidated payroll or settlement journal has hundreds —
// and it holds one request to a thousand round trips and a response of tens of
// kilobytes rather than a megabyte.
const maxPostings = 1000

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
// closed.
type transactionRequest struct {
	Currency    string        `json:"currency"`
	Description string        `json:"description"`
	Postings    []postingBody `json:"postings"`

	// OccurredAt is when the money moved. Absent means now.
	OccurredAt time.Time `json:"occurred_at"`
}

// value hands back what this request carried under a json field name. The amount
// comes off the leg the server named, because the body holds one per posting.
func (t transactionRequest) value(field string, leg *ledger.LegError) string {
	switch field {
	case "currency":
		return t.Currency
	case "description":
		return t.Description
	case "amount_minor":
		if leg != nil {
			return strconv.FormatInt(leg.AmountMinor, 10)
		}
	}
	return ""
}

// net returns the sum of the legs and how many there were. A ledger.Minor, not
// an int64: an entry can carry two amounts the ledger will each hold and still
// net past the width of one of them, and that net is the whole of the answer a
// refusal owes.
func (t transactionRequest) net() (ledger.Minor, int) {
	var sum ledger.Minor
	for _, p := range t.Postings {
		sum = sum.Add(ledger.MinorOf(p.AmountMinor))
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
	Error    string       `json:"error"`
	NetMinor ledger.Minor `json:"net_minor"`
	Postings int          `json:"postings"`
	Expected string       `json:"expected,omitempty"`
	See      string       `json:"see,omitempty"`
}

func (s *server) postTransaction(w http.ResponseWriter, r *http.Request) {
	// First, because it is what decides whether the body should have been sent
	// at all, and because Idempotency-Key is not the thing standing between
	// this endpoint and a browser.
	if !s.declaredJSON(w, r) {
		return
	}

	// Checked before the body: a write that cannot be made safe to retry is not read.
	key := r.Header.Get(keyHeader)
	if key == "" {
		s.write(w, r, http.StatusBadRequest, errorBody{
			Error:     "this endpoint will not take a write it cannot make safe to retry",
			Parameter: keyHeader,
			Expected:  ledger.KeyShape,
			See:       docs.Home,
		})
		return
	}

	r.Body = http.MaxBytesReader(w, r.Body, maxRequestBody)

	dec := json.NewDecoder(r.Body)
	dec.DisallowUnknownFields()

	var req transactionRequest
	if err := dec.Decode(&req); err != nil {
		s.unreadable(w, r, err, transactionFields)
		return
	}

	// The decoder stops at the first json value, so without this `{...}{...}`
	// would post the first entry and say nothing about the second.
	if err := dec.Decode(new(json.RawMessage)); !errors.Is(err, io.EOF) {
		s.write(w, r, http.StatusBadRequest, errorBody{
			Error:    "the request body carries more than one json value",
			Expected: "one json object, and nothing after it",
			See:      docs.Home,
		})
		return
	}

	if n := len(req.Postings); n > maxPostings {
		s.write(w, r, http.StatusUnprocessableEntity, errorBody{
			Error:     "the entry carries more postings than this endpoint takes",
			Parameter: "postings",
			Value:     strconv.Itoa(n),
			Expected:  fmt.Sprintf("at most %d postings", maxPostings),
			See:       docs.Home,
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
// told from a retry.
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

// unreadable answers a body that could not be read as a request. fields is the
// closed set the endpoint accepts.
func (s *server) unreadable(w http.ResponseWriter, r *http.Request, err error, fields []string) {
	var tooBig *http.MaxBytesError
	if errors.As(err, &tooBig) {
		s.write(w, r, http.StatusRequestEntityTooLarge, errorBody{
			Error:    "the request body is larger than this endpoint accepts",
			Expected: fmt.Sprintf("at most %d bytes", tooBig.Limit),
			See:      docs.Home,
		})
		return
	}

	var wrongType *json.UnmarshalTypeError
	if errors.As(err, &wrongType) && wrongType.Field != "" {
		s.write(w, r, http.StatusBadRequest, errorBody{
			Error:     "the field is not the type this endpoint takes",
			Parameter: wrongType.Field,
			Value:     wrongType.Value,
			Expected:  jsonKind(wrongType.Type),
			See:       docs.Home,
		})
		return
	}

	if name := unknownField(err); name != "" {
		s.write(w, r, http.StatusBadRequest, errorBody{
			Error:     "no such field",
			Parameter: name,
			Valid:     fields,
			See:       docs.Home,
		})
		return
	}

	// The decoder says where it stopped, which is the one thing a client
	// cannot work out from a body it thought was json.
	body := errorBody{Error: "the request body is not json this endpoint can read", See: docs.Home}
	var syntax *json.SyntaxError
	if errors.As(err, &syntax) {
		body.Value = fmt.Sprintf("byte %d", syntax.Offset)
	}
	s.write(w, r, http.StatusBadRequest, body)
}

// The field names each endpoint takes, read off the wire types so a refusal
// cannot drift from what the decoder accepts. The decoder does not say how deep
// an unknown field sat, so the set is every name at any depth.
var (
	transactionFields = jsonFields(reflect.TypeFor[transactionRequest]())
	accountFields     = jsonFields(reflect.TypeFor[accountRequest]())
)

func jsonFields(t reflect.Type) []string {
	seen := map[string]bool{}
	var walk func(reflect.Type, map[reflect.Type]bool)
	walk = func(t reflect.Type, done map[reflect.Type]bool) {
		for t.Kind() == reflect.Pointer || t.Kind() == reflect.Slice || t.Kind() == reflect.Array {
			t = t.Elem()
		}
		if t.Kind() != reflect.Struct || done[t] {
			return
		}
		done[t] = true
		for i := range t.NumField() {
			f := t.Field(i)
			name, _, _ := strings.Cut(f.Tag.Get("json"), ",")
			if name == "" || name == "-" {
				continue
			}
			seen[name] = true
			walk(f.Type, done)
		}
	}
	walk(t, map[reflect.Type]bool{})

	out := slices.Collect(maps.Keys(seen))
	slices.Sort(out)
	return out
}

// jsonKind says what a field wanted in the words json uses, rather than in a Go
// type name the client has never heard of.
func jsonKind(t reflect.Type) string {
	if t == reflect.TypeFor[time.Time]() {
		return "an RFC 3339 timestamp"
	}
	switch t.Kind() {
	case reflect.String:
		return "string"
	case reflect.Bool:
		return "boolean"
	case reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64,
		reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64,
		reflect.Float32, reflect.Float64:
		return "number"
	case reflect.Slice, reflect.Array:
		return "array"
	case reflect.Struct, reflect.Map:
		return "object"
	}
	return ""
}

// refusedField reads the field a check refused off the constraint name, which
// Postgres builds as <table>_<column>_check. A name that resolves to no field
// the endpoint takes gives nothing back.
func refusedField(err error, fields []string) string {
	var pgErr *pgconn.PgError
	if !errors.As(err, &pgErr) || pgErr.ConstraintName == "" {
		return ""
	}
	name := strings.TrimPrefix(strings.TrimSuffix(pgErr.ConstraintName, "_check"), pgErr.TableName+"_")
	if slices.Contains(fields, name) {
		return name
	}
	return ""
}

// DisallowUnknownFields reports through errors.New, so the field name is only in
// the text.
var unknownFieldMessage = regexp.MustCompile(`^json: unknown field "(.*)"$`)

func unknownField(err error) string {
	m := unknownFieldMessage.FindStringSubmatch(err.Error())
	if m == nil {
		return ""
	}
	return m[1]
}

// refuse turns the ledger's sentinel errors into a status.
func (s *server) refuse(w http.ResponseWriter, r *http.Request, err error, req transactionRequest, key string) {
	// Which leg, where the refusal named one.
	named := errorBody{}
	var leg *ledger.LegError
	if errors.As(err, &leg) {
		named.Parameter = fmt.Sprintf("postings[%d]", leg.Index)
		named.Code = shown(leg.Account)
	}

	named.See = docs.Home
	switch {
	case cancelled(err):
		s.gaveUp(w, r)
	case errors.Is(err, ledger.ErrKeyReused):
		// What the key was first used for belongs to whoever sent it first.
		s.write(w, r, http.StatusConflict, errorBody{
			Error:     "the idempotency key was used for a different request",
			Parameter: keyHeader,
			Value:     shown(key),
			Expected:  "a key this endpoint has not seen, or the same request it was first sent with",
			See:       docs.Home,
		})

	case errors.Is(err, ledger.ErrBadKey):
		s.write(w, r, http.StatusUnprocessableEntity, errorBody{
			Error:     "the ledger will not hold that idempotency key",
			Parameter: keyHeader,
			Value:     shown(key),
			Expected:  ledger.KeyShape,
			See:       docs.Home,
		})

	case errors.Is(err, ledger.ErrUnbalanced):
		sum, legs := req.net()
		s.write(w, r, http.StatusUnprocessableEntity, unbalancedBody{
			Error:    "the transaction does not balance",
			NetMinor: sum,
			Postings: legs,
			Expected: "postings that net to 0; debits are positive and credits negative",
			See:      docs.Home,
		})

	case errors.Is(err, ledger.ErrUnknownAccount):
		named.Error = "no such account"
		s.write(w, r, http.StatusUnprocessableEntity, named)

	case errors.Is(err, ledger.ErrCurrencyMismatch):
		named.Error = "the account does not hold the transaction's currency"
		named.Value = shown(req.Currency)
		// One account holds one currency, so the closed set here has one member.
		if leg != nil && leg.Holds != "" {
			named.Valid = []string{leg.Holds}
		}
		s.write(w, r, http.StatusUnprocessableEntity, named)

	case errors.Is(err, ledger.ErrRejected):
		// Everything else the schema refuses.
		s.logf("POST %s: %v", r.URL.RequestURI(), err)
		named.Error = "the ledger refused the transaction"
		if field := refusedField(err, transactionFields); field != "" {
			// A leg already names itself in Parameter.
			if leg == nil {
				named.Parameter = field
			}
			// shown, not the value itself: a description the schema refused
			// for its length would otherwise be handed back in full.
			value := req.value(field, leg)
			named.Value = shown(value)
			bound(&named, field, value)
		}
		s.write(w, r, http.StatusUnprocessableEntity, named)

	default:
		s.logf("POST %s: %v", r.URL.RequestURI(), err)
		s.write(w, r, http.StatusInternalServerError, errorBody{Error: "internal error"})
	}
}
