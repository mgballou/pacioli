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

// A badField is one thing a body got wrong: where it sat, what it held there, and
// the rule it broke. A name refused against a closed set carries it in Valid.
type badField struct {
	Parameter string   `json:"parameter"`
	Value     string   `json:"value,omitempty"`
	Expected  string   `json:"expected,omitempty"`
	Valid     []string `json:"valid,omitempty"`

	// unknown is true when the name is the fault rather than the value. json drops it.
	unknown bool
}

// jsonUnmarshaler is what a type implements when it owns its own reading. Such a
// type is a leaf, and this decoder's job is to name the field when it refuses.
var jsonUnmarshaler = reflect.TypeFor[json.Unmarshaler]()

// decode reads r's body into v, a pointer to one of this package's request types,
// reading no more than ceiling bytes of it, and answers r itself when the body
// cannot be read. It reports whether v was filled.
func (s *server) decode(w http.ResponseWriter, r *http.Request, v any, ceiling int64) bool {
	r.Body = http.MaxBytesReader(w, r.Body, ceiling)

	dec := json.NewDecoder(r.Body)
	var raw json.RawMessage
	if err := dec.Decode(&raw); err != nil {
		s.unreadable(w, r, err)
		return false
	}

	// The reader stops at the first json value, so `{...}{...}` would pass unnoticed.
	if err := dec.Decode(new(json.RawMessage)); !errors.Is(err, io.EOF) {
		s.write(w, r, http.StatusBadRequest, errorBody{
			Error:    "the request body carries more than one json value",
			Expected: "one json object, and nothing after it",
			See:      docs.Home,
		})
		return false
	}

	// Said here: a body that is an array is not a field and has no name.
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

// refuseFields answers a body whose fields could not be read. More than one fault
// adds them all under fields, so a client mends a body in one pass.
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

// unreadable answers a body the json reader could not finish, and says where it stopped.
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

	// A body that holds nothing at all; "not json" would not say what is wrong.
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
		// Not enough of it, and the reader gives no offset, so say what it was short of.
		body.Expected = "one complete json object"
	}
	s.write(w, r, http.StatusBadRequest, body)
}

// readValue reads raw into v and returns every fault under it rather than the first.
// path is where v sits in the body, in the words a refusal names it with.
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

// readLeaf reads one value whose type reads itself.
func readLeaf(raw json.RawMessage, v reflect.Value, path string) []badField {
	if err := json.Unmarshal(raw, v.Addr().Interface()); err != nil {
		return []badField{{Parameter: path, Value: sent(raw), Expected: wants(path, v.Type())}}
	}
	return nil
}

// counted is every array field an endpoint holds to a number of elements, by the
// json name it takes the field under.
var counted = map[string]int{"postings": maxPostings}

// readArray reads a json array into a slice element by element, so a fault in the
// third one is named as the third. It reads no further than counted allows, and the
// slice still carries the whole length for the endpoint to refuse on.
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
// struct's own json tags.
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

	// Sorted, so a body with two undefined names is always refused with the same one.
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

// isObject reports whether raw is a json object. Asked rather than left to the
// unmarshal, which reads null into a map without complaint.
func isObject(raw json.RawMessage) bool {
	trimmed := bytes.TrimLeft(raw, " \t\r\n")
	return len(trimmed) > 0 && trimmed[0] == '{'
}

// under names a field inside path, and a field at the top of the body by itself.
func under(path, name string) string {
	if path == "" {
		return name
	}
	return path + "." + name
}

// jsonNamesOf is the names one object takes.
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

// sent is the json the body held where a field was refused. A scalar comes back as
// its own literal, a string keeping the quotes that make it one; a composite comes
// back as the json word for it.
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
