// Package ledgerhttp serves the ledger over HTTP: accounts, balances, the trial
// balance, and the two writes that open an account and post a transaction.
//
// The wire types are this package's own, so renaming a domain field breaks a
// compile rather than a published API.
package ledgerhttp

import (
	"bytes"
	"context"
	"database/sql"
	"database/sql/driver"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"maps"
	"mime"
	"net/http"
	"net/url"
	"slices"
	"strconv"
	"time"
	"unicode/utf8"

	"github.com/mgballou/pacioli/internal/docs"
	"github.com/mgballou/pacioli/internal/ledger"
)

// A Reader lends a read one transaction and takes it back when the read is done.
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
type Pool struct {
	DB *sql.DB

	// Acquire is how long a request may wait for one of the pool's
	// connections before it is refused. Zero is no ceiling; the server refuses
	// zero where the flag is read. DESIGN.md 24.
	Acquire time.Duration
}

// An OverloadError is a request the pool had no connection to lend inside
// Acquire. Nothing was begun, so nothing was written and the request can be
// sent again.
type OverloadError struct{ Ceiling time.Duration }

func (e *OverloadError) Error() string {
	return fmt.Sprintf("no connection out of the pool within %s", e.Ceiling)
}

// conn takes one of the pool's connections, waiting at most p.Acquire for it.
//
// The ceiling is on the wait alone. ctx goes on to carry the transaction, so a
// request that gets a connection keeps the whole of its budget to work in, and
// one that does not is refused now rather than at the end of that budget.
func (p Pool) conn(ctx context.Context) (*sql.Conn, error) {
	if p.Acquire <= 0 {
		return p.DB.Conn(ctx)
	}
	waited, cancel := context.WithTimeout(ctx, p.Acquire)
	defer cancel()

	c, err := p.DB.Conn(waited)
	// The request's own budget running out, or the client going away, is the
	// refusal cancelled already names. This is the queue being too deep.
	if err != nil && ctx.Err() == nil && waited.Err() != nil {
		return nil, &OverloadError{Ceiling: p.Acquire}
	}
	return c, err
}

// Read implements Reader, on a transaction Postgres will not let a handler
// write through. REPEATABLE READ is what makes the one-snapshot claim above
// true: at READ COMMITTED every statement takes its own snapshot, and a
// response built from two of them can be built from two different ledgers.
// DESIGN.md 19.
func (p Pool) Read(ctx context.Context, f func(tx *sql.Tx) error) error {
	c, err := p.conn(ctx)
	if err != nil {
		return fmt.Errorf("take a connection for the read: %w", err)
	}
	defer func() { _ = c.Close() }()

	tx, err := c.BeginTx(ctx, &sql.TxOptions{
		Isolation: sql.LevelRepeatableRead,
		ReadOnly:  true,
	})
	if err != nil {
		return fmt.Errorf("begin the read: %w", err)
	}
	defer func() { _ = tx.Rollback() }()
	return f(tx)
}

// Write implements Writer. Exactly one of Commit and Rollback is reached on
// every path.
//
// READ COMMITTED, and said rather than inherited: the idempotency replay reads
// a row another transaction committed after this one began, and only a snapshot
// taken per statement can see it. DESIGN.md 19.
func (p Pool) Write(ctx context.Context, f func(tx *sql.Tx) error) error {
	c, err := p.conn(ctx)
	if err != nil {
		return fmt.Errorf("take a connection for the write: %w", err)
	}
	defer func() { _ = c.Close() }()

	tx, err := c.BeginTx(ctx, &sql.TxOptions{Isolation: sql.LevelReadCommitted})
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

// Handler returns the ledger's HTTP surface over st. errorLog receives what a
// 500 does not tell the client; nil means log.Default().
func Handler(st Store, errorLog *log.Logger) http.Handler {
	s := &server{ledger: st, errorLog: errorLog}
	mux := http.NewServeMux()
	mux.HandleFunc("GET /v1/accounts", s.accounts)
	mux.HandleFunc("POST /v1/accounts", s.openAccount)
	mux.HandleFunc("GET /v1/accounts/{code}", s.balance)
	mux.HandleFunc("GET /v1/trial-balance", s.trial)
	mux.HandleFunc("POST /v1/transactions", s.postTransaction)
	mux.HandleFunc("GET /v1/transactions/{id}", s.transaction)
	return mux
}

type server struct {
	ledger   Store
	errorLog *log.Logger
}

// jsonMediaType is the one content type the two writes take, and requiring it
// is what keeps a browser from being made to send one.
//
// A cross-origin form or img or fetch that carries no custom header and one of
// three content types — form-encoded, multipart, text/plain — is sent without
// asking anybody first. Requiring application/json puts every write outside
// that set, so a browser has to preflight it, and the mux answers OPTIONS with
// 405. POST /v1/transactions was already outside it, but only because
// Idempotency-Key is a custom header; that is an accident of the retry
// contract, not a control, and this is the control.
const jsonMediaType = "application/json"

// declaredJSON reports whether the request declares a json body, and refuses it
// if not. The Content-Type is the client's, so it is handed back trimmed.
func (s *server) declaredJSON(w http.ResponseWriter, r *http.Request) bool {
	declared := r.Header.Get("Content-Type")
	if media, _, err := mime.ParseMediaType(declared); err == nil && media == jsonMediaType {
		return true
	}
	s.write(w, r, http.StatusUnsupportedMediaType, errorBody{
		Error:     "this endpoint takes only a json body, and the request does not declare one",
		Parameter: "Content-Type",
		Value:     shown(declared),
		Expected:  jsonMediaType,
		See:       docs.Home,
	})
	return false
}

// Deadline gives every request the budget d and answers 503 when it runs out.
// The budget goes on the request's context, so a handler waiting on the
// database is cancelled and gives its connection back, rather than being merely
// disconnected from a client that has already gone.
//
// http.TimeoutHandler throws away the header the inner handler set when it
// fires, so the content type goes on before the request goes in, where the
// answer and the refusal both keep it.
func Deadline(d time.Duration, h http.Handler) http.Handler {
	timed := http.TimeoutHandler(h, d, tookTooLong(d))
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", jsonMediaType)
		timed.ServeHTTP(w, r)
	})
}

// cancelled reports whether an error is the request having run out of budget,
// the client having gone away, or the pooled connection having been left unusable
// by one of those — rather than anything the ledger refused.
//
// driver.ErrBadConn is here because cancelling a query is what leaves a
// connection unusable, and database/sql hands it back only after retrying on
// fresh ones. It means the transaction was never begun, so nothing was written
// and nothing can have been half written.
func cancelled(err error) bool {
	return errors.Is(err, context.Canceled) ||
		errors.Is(err, context.DeadlineExceeded) ||
		errors.Is(err, driver.ErrBadConn)
}

// gaveUp answers a request that never got as far as being refused. Deadline has
// usually answered already and this body is thrown away; a handler that finishes
// a moment before its budget instead is what this is for, and what it must not
// be is a 500, because nothing internal went wrong and the same request sent
// again is the right answer.
func (s *server) gaveUp(w http.ResponseWriter, r *http.Request) {
	s.write(w, r, http.StatusServiceUnavailable, errorBody{
		Error:    "the ledger did not get to this request and nothing was written",
		Expected: "the same request again; a write carries its idempotency key, so sending it twice cannot post it twice",
		See:      docs.Home,
	})
}

// notReached answers a request the ledger never got to, and reports whether it
// did. Every handler asks this before it reads an error as a refusal, because
// neither of these is one.
func (s *server) notReached(w http.ResponseWriter, r *http.Request, err error) bool {
	var busy *OverloadError
	switch {
	case errors.As(err, &busy):
		s.busy(w, r, busy.Ceiling)
	case cancelled(err):
		s.gaveUp(w, r)
	default:
		return false
	}
	return true
}

// busy answers a request the pool had no connection for. It carries Retry-After
// because it is the one refusal here that says when: the queue was full for the
// ceiling, so that is how long the client is asked to leave it. DESIGN.md 24.
func (s *server) busy(w http.ResponseWriter, r *http.Request, ceiling time.Duration) {
	w.Header().Set("Retry-After", retryAfter(ceiling))
	s.write(w, r, http.StatusServiceUnavailable, errorBody{
		Error:    "the ledger is busy and had no free connection for this request, so nothing was written",
		Expected: "the same request again after " + ceiling.String() + "; a write carries its idempotency key, so sending it twice cannot post it twice",
		See:      docs.Home,
	})
}

// retryAfter is the ceiling in the whole seconds Retry-After is counted in,
// rounded up and never zero: a client told to come back in no time at all comes
// straight back.
func retryAfter(d time.Duration) string {
	seconds := (d + time.Second - 1) / time.Second
	if seconds < 1 {
		seconds = 1
	}
	return strconv.FormatInt(int64(seconds), 10)
}

// tookTooLong is the body a request that ran out of budget is answered with,
// in the shape every other refusal has.
func tookTooLong(d time.Duration) string {
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetIndent("", "  ")
	// errorBody is four strings, so this cannot fail; the check is here
	// because dropping the error would be the thing that hides it if it did.
	if err := enc.Encode(errorBody{
		Error:    "the ledger did not answer in time and the request was cancelled",
		Expected: "an answer within " + d.String(),
		See:      docs.Home,
	}); err != nil {
		return `{"error":"the ledger did not answer in time and the request was cancelled"}`
	}
	return buf.String()
}

// balanceBody is one account's position, in minor units.
type balanceBody struct {
	Account      string       `json:"account"`
	Name         string       `json:"name"`
	Kind         string       `json:"kind"`
	Currency     string       `json:"currency"`
	BalanceMinor ledger.Minor `json:"balance_minor"`
	Postings     int64        `json:"postings"`
}

// accountListBody wraps the rows in an object, which is what let a page be
// added to it.
type accountListBody struct {
	Accounts []balanceBody `json:"accounts"`

	// Next is the address of the page after this one, absent on the last page.
	Next string `json:"next,omitempty"`
}

// trialBody is one currency's side totals.
type trialBody struct {
	Currency     string       `json:"currency"`
	DebitsMinor  ledger.Minor `json:"debits_minor"`
	CreditsMinor ledger.Minor `json:"credits_minor"`
	NetMinor     ledger.Minor `json:"net_minor"`
	Balanced     bool         `json:"balanced"`
	Accounts     int64        `json:"accounts"`
	Postings     int64        `json:"postings"`
}

// trialListBody wraps the rows in an object, which a bare array could not be
// added to later.
type trialListBody struct {
	Trial []trialBody `json:"trial"`
}

// errorBody is what almost every refusal looks like: the rule, and what was
// given against it.
type errorBody struct {
	Error     string `json:"error"`
	Code      string `json:"code,omitempty"`
	Parameter string `json:"parameter,omitempty"`

	// Value is what the request carried under Parameter, handed straight back,
	// or where in the body the reading of it stopped.
	Value string `json:"value,omitempty"`

	// Expected is the rule in words, for a set that is open but shaped.
	Expected string `json:"expected,omitempty"`

	// Characters is how long Value was before it was trimmed to fit, set only
	// where the length is what broke the rule. Value alone cannot say how far
	// over the limit a 100,000-character description was.
	Characters int `json:"characters,omitempty"`

	// Valid is the whole set, for a set that is closed.
	Valid []string `json:"valid,omitempty"`

	// Fields is every field a body got wrong, set only where it got more than
	// one wrong. Parameter, Value, Expected and Valid name the first of them,
	// and it is in here too, so a client that reads this list reads all of it.
	Fields []badField `json:"fields,omitempty"`

	// See is where the rule is written down. It is the same for every refusal.
	See string `json:"see,omitempty"`
}

// validKinds takes the closed set off a *ledger.UnknownKindError, and nothing
// off anything else.
func validKinds(err error) []string {
	var unknown *ledger.UnknownKindError
	if errors.As(err, &unknown) {
		return unknown.Valid
	}
	return nil
}

// bounded is every field the schema holds to a length, with the rule in words
// and the characters it allows. The names are the schema's column names, which
// are the json field names the endpoints take.
var bounded = map[string]struct {
	shape string
	most  int
}{
	"code":        {ledger.CodeShape, ledger.MaxCode},
	"name":        {ledger.NameShape, ledger.MaxName},
	"description": {ledger.DescriptionShape, ledger.MaxDescription},
}

// bound fills in the rule a bounded field is held to, and how long the value
// was when the length is what broke it. A field the schema does not bound by
// length leaves the refusal exactly as it was.
func bound(body *errorBody, field, value string) {
	b, ok := bounded[field]
	if !ok {
		return
	}
	body.Expected = b.shape
	if n := utf8.RuneCountInString(value); n > b.most {
		body.Characters = n
	}
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
		s.fail(w, r, err, errorBody{Code: shown(code)})
		return
	}

	s.write(w, r, http.StatusOK, balanceOf(b))
}

// accountsQuery is the closed set of parameters GET /v1/accounts takes.
var accountsQuery = map[string]bool{"currency": true, "kind": true, "limit": true, "after": true}

// The page GET /v1/accounts answers with. defaultPage is what a request that
// names no size gets; maxPage is the most one can ask for.
//
// The chart grows and the schema puts no ceiling on it, so the whole of it was
// the answer: 200,000 accounts came back as 39 MB. A thousand is the number
// this package already holds an entry's legs to, and a thousand accounts is a
// response of a few hundred kilobytes. DESIGN.md 23.
const (
	defaultPage = 100
	maxPage     = 1000
)

// pageShape puts the page size into words a client can act on, the way
// ledger.CodeShape does for a code.
var pageShape = fmt.Sprintf("a whole number from 1 to %d", maxPage)

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

	if bad := unknown(q); len(bad) > 0 {
		s.write(w, r, http.StatusBadRequest, errorBody{
			Error:     "no such query parameter",
			Parameter: bad[0],
			Valid:     queryParameters(),
			See:       docs.Home,
		})
		return
	}

	page, ok := s.pageSize(w, r, q)
	if !ok {
		return
	}

	f := ledger.AccountFilter{
		Currency: q.Get("currency"),
		Kind:     q.Get("kind"),
		After:    q.Get("after"),
		// One past the page, so the answer knows whether there is another page
		// without counting the chart.
		Limit: page + 1,
	}

	var rows []ledger.Balance
	err = s.ledger.Read(r.Context(), func(tx *sql.Tx) error {
		var err error
		rows, err = ledger.Balances(r.Context(), tx, f)
		return err
	})
	if err != nil {
		s.fail(w, r, err, errorBody{Parameter: "kind", Value: shown(f.Kind)})
		return
	}

	more := len(rows) > page
	if more {
		rows = rows[:page]
	}

	// Not nil: a question that matched nothing should encode as [], not null.
	body := accountListBody{Accounts: make([]balanceBody, 0, len(rows))}
	for _, b := range rows {
		body.Accounts = append(body.Accounts, balanceOf(b))
	}
	if more {
		body.Next = nextPage(f, page, rows[len(rows)-1].Account)
	}
	s.write(w, r, http.StatusOK, body)
}

// pageSize reads the page size off the query. It is checked before the ledger
// is asked, so a size this endpoint will not answer with costs a read of
// nothing.
func (s *server) pageSize(w http.ResponseWriter, r *http.Request, q url.Values) (int, bool) {
	asked := q.Get("limit")
	if asked == "" {
		return defaultPage, true
	}
	page, err := strconv.Atoi(asked)
	if err != nil || page < 1 || page > maxPage {
		s.write(w, r, http.StatusBadRequest, errorBody{
			Error:     "that is not a page size this endpoint answers with",
			Parameter: "limit",
			Value:     shown(asked),
			Expected:  pageShape,
			See:       docs.Home,
		})
		return 0, false
	}
	return page, true
}

// nextPage is where the rest of the answer is: the question that was asked, the
// size it was answered at, and the last code this page reached.
func nextPage(f ledger.AccountFilter, page int, last string) string {
	q := url.Values{}
	if f.Currency != "" {
		q.Set("currency", f.Currency)
	}
	if f.Kind != "" {
		q.Set("kind", f.Kind)
	}
	q.Set("limit", strconv.Itoa(page))
	q.Set("after", last)
	return "/v1/accounts?" + q.Encode()
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

// balanceOf converts a domain balance to the wire type.
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

// trial serves one row per currency the chart holds, and takes no page. The
// schema's currency CHECK is three capitals, so there are at most 17,576 rows
// however large the chart grows. DESIGN.md 23.
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

// fail turns an error from internal/ledger into a status. named carries whatever
// part of the request is worth handing back.
func (s *server) fail(w http.ResponseWriter, r *http.Request, err error, named errorBody) {
	if s.notReached(w, r, err) {
		return
	}

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
	case errors.Is(err, ledger.ErrBadTransactionID):
		// 400, not 404: the path segment could not name a transaction, so the
		// ledger was never asked whether one holds it. DESIGN.md 10.
		named.Error = "that is not a transaction id"
		named.Expected = ledger.IDShape
		s.write(w, r, http.StatusBadRequest, named)
		return
	case errors.Is(err, ledger.ErrUnknownTransaction):
		named.Error = "no such transaction"
		s.write(w, r, http.StatusNotFound, named)
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
	// ErrHandlerTimeout is Deadline having already answered this request, which
	// is a refusal it accounted for and not a failure to log.
	if _, err := w.Write(buf.Bytes()); err != nil && !errors.Is(err, http.ErrHandlerTimeout) {
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
