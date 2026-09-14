package ledgerhttp

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"maps"
	"net/http"
	"reflect"
	"slices"
	"strconv"
	"strings"

	"github.com/mgballou/pacioli/internal/docs"
)

// This file reads a request body a field at a time. DESIGN.md 20 argues it.

// A badField is one thing a body got wrong: where it sat, what it held there,
// and the rule it broke. The set a name is refused against goes in Valid, the
// same way every other closed-set refusal carries one.
type badField struct {
	Parameter string   `json:"parameter"`
	Value     string   `json:"value,omitempty"`
	Expected  string   `json:"expected,omitempty"`
	Valid     []string `json:"valid,omitempty"`

	// unknown is true when the name is the fault rather than the value, which
	// is the other sentence a single fault is refused with. json drops it.
	unknown bool
}

// jsonUnmarshaler is what a type implements when it owns its own reading.
// time.Time is the one this package has, and such a type is a leaf: what it
// takes is its business, and this decoder's job is to name the field when it
// refuses.
var jsonUnmarshaler = reflect.TypeFor[json.Unmarshaler]()

// decode reads r's body into v, a pointer to one of this package's request
// types, reading no more than ceiling bytes of it, and answers r itself when
// the body cannot be read. It reports whether v was filled.
//
// The ceiling is the endpoint's, because it is the endpoint that knows what it
// takes. DESIGN.md 25.
//
// Handing the whole body to encoding/json is two lines and was what this was.
// What that will not do is name the field when the field owns its own reading:
// occurred_at is a time.Time, and a body carrying "tuesday" came back "the
// request body is not json this endpoint can read", of a body that was good
// json throughout. Nor will it look for a second bad field once it has found
// the first.
func (s *server) decode(w http.ResponseWriter, r *http.Request, v any, ceiling int64) bool {
	r.Body = http.MaxBytesReader(w, r.Body, ceiling)

	dec := json.NewDecoder(r.Body)
	var raw json.RawMessage
	if err := dec.Decode(&raw); err != nil {
		s.unreadable(w, r, err)
		return false
	}

	// The reader stops at the first json value, so without this `{...}{...}`
	// would take the first and say nothing about the second.
	if err := dec.Decode(new(json.RawMessage)); !errors.Is(err, io.EOF) {
		s.write(w, r, http.StatusBadRequest, errorBody{
			Error:    "the request body carries more than one json value",
			Expected: "one json object, and nothing after it",
			See:      docs.Home,
		})
		return false
	}

	// Said here rather than left to readObject, because a whole body that is
	// an array is not a field and has no name to be refused under.
	if !isObject(raw) {
		s.write(w, r, http.StatusBadRequest, errorBody{
			Error:    "the request body is json this endpoint does not take",
			Value:    sent(raw),
			Expected: "one json object",
			See:      docs.Home,
		})
		return false
	}

	if bad := readObject(raw, reflect.ValueOf(v).Elem(), ""); len(bad) > 0 {
		s.refuseFields(w, r, bad)
		return false
	}
	return true
}

// refuseFields answers a body whose fields could not be read. One fault keeps
// the shape a refusal has always had — the name, the value and the rule at the
// top level. More than one adds them all under fields, so a client mends a body
// in one pass rather than one round trip per field.
func (s *server) refuseFields(w http.ResponseWriter, r *http.Request, bad []badField) {
	body := errorBody{
		Parameter: bad[0].Parameter,
		Value:     bad[0].Value,
		Expected:  bad[0].Expected,
		Valid:     bad[0].Valid,
		See:       docs.Home,
	}
	switch {
	case len(bad) > 1:
		body.Error = "the request body carries fields this endpoint cannot read"
		body.Fields = bad
	case bad[0].unknown:
		body.Error = "no such field"
	default:
		body.Error = "the field is not the shape this endpoint takes"
	}
	s.write(w, r, http.StatusBadRequest, body)
}

// unreadable answers a body the json reader could not finish. A body that is
// genuinely not json still says so, and says where the reading stopped, which
// is the one thing a client cannot work out from a body it thought was json.
func (s *server) unreadable(w http.ResponseWriter, r *http.Request, err error) {
	var tooBig *http.MaxBytesError
	if errors.As(err, &tooBig) {
		s.write(w, r, http.StatusRequestEntityTooLarge, errorBody{
			Error:    "the request body is larger than this endpoint accepts",
			Expected: fmt.Sprintf("at most %d bytes", tooBig.Limit),
			See:      docs.Home,
		})
		return
	}

	// A body that holds nothing at all, which "not json" describes without
	// telling the sender the one thing that is wrong with it.
	if errors.Is(err, io.EOF) {
		s.write(w, r, http.StatusBadRequest, errorBody{
			Error:    "the request body is empty",
			Expected: "one json object",
			See:      docs.Home,
		})
		return
	}

	body := errorBody{Error: "the request body is not json this endpoint can read", See: docs.Home}
	var syntax *json.SyntaxError
	switch {
	case errors.As(err, &syntax):
		body.Value = fmt.Sprintf("byte %d", syntax.Offset)
	case errors.Is(err, io.ErrUnexpectedEOF):
		// Nothing in the body is wrong; there is not enough of it. The reader
		// reports no offset for this, so the body says what it was short of.
		body.Expected = "one complete json object"
	}
	s.write(w, r, http.StatusBadRequest, body)
}

// readValue reads raw into v and returns every fault under it rather than the
// first. path is where v sits in the body, in the words a refusal names it with.
func readValue(raw json.RawMessage, v reflect.Value, path string) []badField {
	t := v.Type()
	switch {
	case reflect.PointerTo(t).Implements(jsonUnmarshaler):
	case t.Kind() == reflect.Struct:
		return readObject(raw, v, path)
	case t.Kind() == reflect.Slice:
		return readArray(raw, v, path)
	}
	return readLeaf(raw, v, path)
}

// readLeaf reads one value whose type reads itself, and turns a refusal from it
// into the field's name, what the body held, and the rule in words.
func readLeaf(raw json.RawMessage, v reflect.Value, path string) []badField {
	if err := json.Unmarshal(raw, v.Addr().Interface()); err != nil {
		return []badField{{Parameter: path, Value: sent(raw), Expected: wants(path, v.Type())}}
	}
	return nil
}

// counted is every array field an endpoint holds to a number of elements, by
// the json name the endpoint takes it under. Flat for the reason bounded and
// shaped are, and read by both endpoints, because both read a body through
// decode.
var counted = map[string]int{"postings": maxPostings}

// readArray reads a json array into a slice, element by element, so a fault in
// the third one is named as the third.
//
// It reads no further than counted allows. Past that the answer is already the
// count — the endpoint refuses it, and says so with the whole length, which the
// slice still carries — so reading the rest decides nothing and costs
// everything: a body of 5,500 legs was walked in full to be told it may carry a
// thousand. DESIGN.md 25.
func readArray(raw json.RawMessage, v reflect.Value, path string) []badField {
	var items []json.RawMessage
	if err := json.Unmarshal(raw, &items); err != nil {
		return []badField{{Parameter: path, Value: sent(raw), Expected: wants(path, v.Type())}}
	}
	v.Set(reflect.MakeSlice(v.Type(), len(items), len(items)))

	read := len(items)
	if most, ok := counted[leaf(path)]; ok && read > most {
		read = most
	}

	var bad []badField
	for i, item := range items[:read] {
		bad = append(bad, readValue(item, v.Index(i), fmt.Sprintf("%s[%d]", path, i))...)
	}
	return bad
}

// readObject reads a json object into a struct, taking the field names off the
// struct's own json tags, so the set an endpoint accepts cannot drift from the
// set it refuses against.
func readObject(raw json.RawMessage, v reflect.Value, path string) []badField {
	var got map[string]json.RawMessage
	if !isObject(raw) || json.Unmarshal(raw, &got) != nil {
		return []badField{{Parameter: path, Value: sent(raw), Expected: "object"}}
	}

	t := v.Type()
	defined := map[string]bool{}
	var bad []badField
	for i := range t.NumField() {
		f := t.Field(i)
		name, _, _ := strings.Cut(f.Tag.Get("json"), ",")
		if !f.IsExported() || name == "" || name == "-" {
			continue
		}
		defined[name] = true
		if item, ok := got[name]; ok {
			bad = append(bad, readValue(item, v.Field(i), under(path, name))...)
		}
	}

	// Sorted, so a body carrying two names this endpoint does not define is
	// always refused with the same one first. unknown() sorts for the same
	// reason.
	for _, name := range slices.Sorted(maps.Keys(got)) {
		if !defined[name] {
			bad = append(bad, badField{
				Parameter: under(path, name),
				Valid:     jsonNamesOf(t),
				unknown:   true,
			})
		}
	}
	return bad
}

// isObject reports whether raw is a json object. It is asked rather than left
// to the unmarshal, which reads null into a map without complaint and would
// take a body of `null` for an object with no fields in it.
func isObject(raw json.RawMessage) bool {
	trimmed := bytes.TrimLeft(raw, " \t\r\n")
	return len(trimmed) > 0 && trimmed[0] == '{'
}

// under names a field inside path, and names a field at the top of the body by
// itself.
func under(path, name string) string {
	if path == "" {
		return name
	}
	return path + "." + name
}

// jsonNamesOf is the names one object takes, which is the closed set a name it
// does not take is refused against.
func jsonNamesOf(t reflect.Type) []string {
	var out []string
	for i := range t.NumField() {
		f := t.Field(i)
		name, _, _ := strings.Cut(f.Tag.Get("json"), ",")
		if !f.IsExported() || name == "" || name == "-" {
			continue
		}
		out = append(out, name)
	}
	slices.Sort(out)
	return out
}

// sent is the json the body held where a field was refused, so a client can
// match the refusal against what it typed. A scalar is handed back as its own
// literal, a string keeping the quotes that make it one — otherwise the number
// 100 and the string "100" are refused with the same four characters, and this
// endpoint refuses one and takes the other.
//
// A composite is handed back as the json word for it. An object of unknown size
// is not something to quote into a one-line refusal, and the fault with one is
// that it is an object at all.
//
// This is the json layer's answer, so it differs from the value a schema
// refusal hands back: that one is the string the ledger was given, after this
// decoder read it.
func sent(raw json.RawMessage) string {
	trimmed := bytes.TrimSpace(raw)
	if len(trimmed) == 0 {
		return ""
	}
	switch trimmed[0] {
	case '{':
		return "object"
	case '[':
		return "array"
	case '"':
		var s string
		if json.Unmarshal(trimmed, &s) != nil {
			return "string"
		}
		return strconv.Quote(shown(s))
	}
	// A number, true, false or null, each of which is its own literal.
	return shown(string(trimmed))
}
