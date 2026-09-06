// Package ledgerhttp serves the ledger over HTTP: accounts, balances, the trial
// balance, and the two writes that open an account and post a transaction.
//
// A read runs on a transaction opened READ ONLY. A write runs on its own, and
// commits only if the handler got through. The wire types are this package's
// own, so renaming a domain field breaks a compile rather than a published API.
package ledgerhttp

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"maps"
	"net/http"
	"net/url"
	"slices"
	"strconv"
	"unicode/utf8"

	"github.com/mgballou/pacioli/internal/docs"
	"github.com/mgballou/pacioli/internal/ledger"
)

// A Reader lends a read one transaction and takes it back when the read is done.
// Pool is what a server uses; a test supplies the transaction it is already in.
type Reader interface {
	Read(ctx context.Context, f func(tx *sql.Tx) error) error
}

// A Writer lends a change one transaction and decides its fate: committed if f
// returns nil, rolled back if it returns anything else.
type Writer interface {
	Write(ctx context.Context, f func(tx *sql.Tx) error) error
}

// A Store is both, and it is what Handler is given.
type Store interface {
	Reader
	Writer
}

// Pool works through a connection pool, one transaction per call, so a response
// built from two statements is built from one snapshot of the ledger.
type Pool struct{ DB *sql.DB }

// Read implements Reader, on a transaction Postgres will not let a handler
// write through.
func (p Pool) Read(ctx context.Context, f func(tx *sql.Tx) error) error {
	tx, err := p.DB.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		return fmt.Errorf("begin the read: %w", err)
	}
	defer func() { _ = tx.Rollback() }()
	return f(tx)
}

// Write implements Writer. Both ways out are written out rather than deferred,
// so exactly one of Commit and Rollback is reached on every path.
func (p Pool) Write(ctx context.Context, f func(tx *sql.Tx) error) error {
	tx, err := p.DB.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin the write: %w", err)
	}
	if err := f(tx); err != nil {
		if rbErr := tx.Rollback(); rbErr != nil && !errors.Is(rbErr, sql.ErrTxDone) {
			return errors.Join(err, fmt.Errorf("roll back the write: %w", rbErr))
		}
		return err
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit the write: %w", err)
	}
	return nil
}

// Handler returns the ledger's HTTP surface over the store st lends transactions
// on. errorLog receives what a 500 does not tell the client; nil means
// log.Default().
func Handler(st Store, errorLog *log.Logger) http.Handler {
	s := &server{ledger: st, errorLog: errorLog}
	mux := http.NewServeMux()
	mux.HandleFunc("GET /v1/accounts", s.accounts)
	mux.HandleFunc("POST /v1/accounts", s.openAccount)
	mux.HandleFunc("GET /v1/accounts/{code}", s.balance)
	mux.HandleFunc("GET /v1/trial-balance", s.trial)
	mux.HandleFunc("POST /v1/transactions", s.postTransaction)
	return mux
}

type server struct {
	ledger   Store
	errorLog *log.Logger
}

// balanceBody is one account's position, in minor units.
type balanceBody struct {
	Account      string `json:"account"`
	Name         string `json:"name"`
	Kind         string `json:"kind"`
	Currency     string `json:"currency"`
	BalanceMinor int64  `json:"balance_minor"`
	Postings     int64  `json:"postings"`
}

// accountListBody wraps the rows in an object.
type accountListBody struct {
	Accounts []balanceBody `json:"accounts"`
}

// trialBody is one currency's side totals. Balanced is carried rather than left
// for the client to derive.
type trialBody struct {
	Currency     string `json:"currency"`
	DebitsMinor  int64  `json:"debits_minor"`
	CreditsMinor int64  `json:"credits_minor"`
	NetMinor     int64  `json:"net_minor"`
	Balanced     bool   `json:"balanced"`
	Accounts     int64  `json:"accounts"`
	Postings     int64  `json:"postings"`
}

// trialListBody wraps the rows in an object. A bare top-level array is a shape
// nothing can be added to later.
type trialListBody struct {
	Trial []trialBody `json:"trial"`
}

// errorBody is what almost every refusal looks like. The message says the rule;
// the rest says what was given, what would have been taken, and where the rule
// is written down.
type errorBody struct {
	Error     string `json:"error"`
	Code      string `json:"code,omitempty"`
	Parameter string `json:"parameter,omitempty"`

	// Value is what the request carried under Parameter, handed straight back,
	// or where in the body the reading of it stopped.
	Value string `json:"value,omitempty"`

	// Expected is the rule in words, for a set that is open but shaped.
	Expected string `json:"expected,omitempty"`

	// Valid is the whole set, for a set that is closed.
	Valid []string `json:"valid,omitempty"`

	// See is where the rule is written down. It is the same for every refusal.
	See string `json:"see,omitempty"`
}

// validKinds takes the closed set off the ledger's typed error rather than out
// of its text, and gives nothing where the refusal was not about a kind.
func validKinds(err error) []string {
	var unknown *ledger.UnknownKindError
	if errors.As(err, &unknown) {
		return unknown.Valid
	}
	return nil
}

// shown trims a value a client chose to something a refusal can carry back.
func shown(s string) string {
	const most = 80
	if utf8.RuneCountInString(s) <= most {
		return s
	}
	return string([]rune(s)[:most]) + "\u2026"
}

func (s *server) balance(w http.ResponseWriter, r *http.Request) {
	code := r.PathValue("code")

	var b ledger.Balance
	err := s.ledger.Read(r.Context(), func(tx *sql.Tx) error {
		var err error
		b, err = ledger.BalanceOf(r.Context(), tx, code)
		return err
	})
	if err != nil {
		s.fail(w, r, err, errorBody{Code: code})
		return
	}

	s.write(w, r, http.StatusOK, balanceOf(b))
}

// accountsQuery is the closed set of parameters GET /v1/accounts takes.
var accountsQuery = map[string]bool{"currency": true, "kind": true}

func (s *server) accounts(w http.ResponseWriter, r *http.Request) {
	// Not r.URL.Query(): it drops the error, so `?%zz=1` would arrive as no
	// parameters and be answered with the whole chart.
	q, err := url.ParseQuery(r.URL.RawQuery)
	if err != nil {
		s.write(w, r, http.StatusBadRequest, errorBody{
			Error: "the query string could not be read",
			Value: shown(r.URL.RawQuery),
			See:   docs.Home,
		})
		return
	}

	// A parameter this endpoint does not define is refused, not ignored:
	// `?curency=GBP` answered with every account looks like an answer.
	if bad := unknown(q); len(bad) > 0 {
		s.write(w, r, http.StatusBadRequest, errorBody{
			Error:     "no such query parameter",
			Parameter: bad[0],
			Valid:     queryParameters(),
			See:       docs.Home,
		})
		return
	}

	f := ledger.AccountFilter{Currency: q.Get("currency"), Kind: q.Get("kind")}

	var rows []ledger.Balance
	err = s.ledger.Read(r.Context(), func(tx *sql.Tx) error {
		var err error
		rows, err = ledger.Balances(r.Context(), tx, f)
		return err
	})
	if err != nil {
		s.fail(w, r, err, errorBody{Parameter: "kind", Value: f.Kind})
		return
	}

	// Not nil: a question that matched nothing should encode as [], not null.
	body := accountListBody{Accounts: make([]balanceBody, 0, len(rows))}
	for _, b := range rows {
		body.Accounts = append(body.Accounts, balanceOf(b))
	}
	s.write(w, r, http.StatusOK, body)
}

// queryParameters is the closed set a refusal hands back, read off the same map
// the refusal was made from.
func queryParameters() []string {
	out := slices.Collect(maps.Keys(accountsQuery))
	slices.Sort(out)
	return out
}

// unknown returns the parameter names accountsQuery does not define, sorted so
// the same request is always refused with the same one.
func unknown(q url.Values) []string {
	var out []string
	for name := range q {
		if !accountsQuery[name] {
			out = append(out, name)
		}
	}
	slices.Sort(out)
	return out
}

// balanceOf converts a domain balance to the wire type. It is not a method on
// ledger.Balance, so the domain keeps knowing nothing about json.
func balanceOf(b ledger.Balance) balanceBody {
	return balanceBody{
		Account:      b.Account,
		Name:         b.Name,
		Kind:         b.Kind,
		Currency:     b.Currency,
		BalanceMinor: b.AmountMinor,
		Postings:     b.Postings,
	}
}

func (s *server) trial(w http.ResponseWriter, r *http.Request) {
	var rows []ledger.Trial
	err := s.ledger.Read(r.Context(), func(tx *sql.Tx) error {
		var err error
		rows, err = ledger.TrialBalance(r.Context(), tx)
		return err
	})
	if err != nil {
		s.fail(w, r, err, errorBody{})
		return
	}

	// Not nil: an empty chart should encode as [], not null.
	body := trialListBody{Trial: make([]trialBody, 0, len(rows))}
	for _, t := range rows {
		body.Trial = append(body.Trial, trialBody{
			Currency:     t.Currency,
			DebitsMinor:  t.DebitsMinor,
			CreditsMinor: t.CreditsMinor,
			NetMinor:     t.NetMinor,
			Balanced:     t.Balanced(),
			Accounts:     t.Accounts,
			Postings:     t.Postings,
		})
	}
	s.write(w, r, http.StatusOK, body)
}

// fail turns an error from internal/ledger into a status, switching on that
// package's sentinels rather than on message text. named carries whatever part
// of the request is worth handing back with the refusal.
func (s *server) fail(w http.ResponseWriter, r *http.Request, err error, named errorBody) {
	named.See = docs.Home
	switch {
	case errors.Is(err, ledger.ErrUnknownAccount):
		named.Error = "no such account"
		s.write(w, r, http.StatusNotFound, named)
		return
	case errors.Is(err, ledger.ErrUnknownKind):
		named.Valid = validKinds(err)
		named.Error = "no such account kind"
		s.write(w, r, http.StatusBadRequest, named)
		return
	}
	// RequestURI rather than Path: on a list the query string is the half of
	// the request that can be wrong.
	s.logf("%s %s: %v", r.Method, r.URL.RequestURI(), err)
	s.write(w, r, http.StatusInternalServerError, errorBody{Error: "internal error"})
}

// write encodes into a buffer before it touches the ResponseWriter, so an encode
// that fails cannot arrive as a 200 with a truncated body.
func (s *server) write(w http.ResponseWriter, r *http.Request, status int, v any) {
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	// Indented for a human reading a transcript.
	enc.SetIndent("", "  ")
	if err := enc.Encode(v); err != nil {
		s.logf("%s %s: encode %T: %v", r.Method, r.URL.Path, v, err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}

	// No charset parameter: JSON is UTF-8 by definition.
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Content-Length", strconv.Itoa(buf.Len()))
	w.WriteHeader(status)
	if _, err := w.Write(buf.Bytes()); err != nil {
		s.logf("%s %s: write body: %v", r.Method, r.URL.Path, err)
	}
}

func (s *server) logf(format string, args ...any) {
	if s.errorLog != nil {
		s.errorLog.Printf(format, args...)
		return
	}
	log.Printf(format, args...)
}
