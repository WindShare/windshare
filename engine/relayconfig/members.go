package relayconfig

import (
	"encoding"
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"strconv"
	"strings"
	"sync"
)

// ErrRepeatedMember reports an object that names a member twice. It is
// distinct from other ErrConfig causes because jq and python -m json.tool keep
// one repetition silently, so a user checking the file with them would not see
// the problem unless the error names the member.
var ErrRepeatedMember = fmt.Errorf("%w; an object member appears more than once", ErrConfig)

// repeatedMemberError names only members and their object, never values: the
// repeated member is usually a URL or an access key. Every name it holds
// matches a schema field, so the message stays short and single-line; names
// are quoted ASCII-only to make look-alikes such as U+212A visible.
type repeatedMemberError struct {
	name, first string
	// object holds the path to the object innermost first. It is filled only
	// while the error unwinds, so a scan that succeeds builds no paths.
	object []string
}

func (failure *repeatedMemberError) Error() string {
	message := fmt.Sprintf("%s; member %+q appears more than once in %s", ErrConfig, failure.name, failure.objectPath())
	if failure.first != failure.name {
		message += fmt.Sprintf(" (also written as %+q; member names match case-insensitively)", failure.first)
	}
	return message
}

func (failure *repeatedMemberError) objectPath() string {
	if len(failure.object) == 0 {
		return "the top-level object"
	}
	var path strings.Builder
	for i := len(failure.object) - 1; i >= 0; i-- {
		segment := failure.object[i]
		if path.Len() > 0 && !strings.HasPrefix(segment, "[") {
			path.WriteByte('.')
		}
		path.WriteString(segment)
	}
	return path.String()
}

func (failure *repeatedMemberError) Unwrap() error { return ErrRepeatedMember }

// within records the segment of the object path that the error unwinds through.
func within(err error, segment string) error {
	var failure *repeatedMemberError
	if errors.As(err, &failure) {
		failure.object = append(failure.object, segment)
	}
	return err
}

// schemaNode is what Decode accepts at one position of the document, derived
// from the document type so the scan cannot drift from it. A node is opaque
// (decoded by its own UnmarshalJSON, such as turn.accounts[].credentials as
// json.RawMessage), an object with fields, an array with an element, or a
// scalar (neither).
type schemaNode struct {
	opaque  bool
	fields  map[string]schemaField
	element *schemaNode
}

// schemaField is keyed by foldMemberName of its JSON name.
type schemaField struct {
	name string
	node *schemaNode
}

var documentSchema = sync.OnceValue(func() *schemaNode {
	return schemaOf(reflect.TypeFor[document](), map[reflect.Type]*schemaNode{})
})

var (
	jsonUnmarshaler = reflect.TypeFor[json.Unmarshaler]()
	textUnmarshaler = reflect.TypeFor[encoding.TextUnmarshaler]()
)

func schemaOf(t reflect.Type, known map[reflect.Type]*schemaNode) *schemaNode {
	for t.Kind() == reflect.Pointer {
		t = t.Elem()
	}
	if node, ok := known[t]; ok {
		return node
	}
	node := &schemaNode{}
	known[t] = node
	pointer := reflect.PointerTo(t)
	switch {
	case pointer.Implements(jsonUnmarshaler) || pointer.Implements(textUnmarshaler):
		node.opaque = true
	case t.Kind() == reflect.Struct:
		node.fields = map[string]schemaField{}
		addFields(node.fields, t, known)
	case t.Kind() == reflect.Slice || t.Kind() == reflect.Array:
		node.element = schemaOf(t.Elem(), known)
	case t.Kind() == reflect.Map || t.Kind() == reflect.Interface:
		node.opaque = true
	}
	return node
}

// addFields follows encoding/json's naming: the tag name, else the Go name;
// "-" omits a field; untagged embedded structs contribute their fields.
func addFields(fields map[string]schemaField, t reflect.Type, known map[reflect.Type]*schemaNode) {
	for _, field := range reflect.VisibleFields(t) {
		if len(field.Index) > 1 || !field.IsExported() && !field.Anonymous {
			continue
		}
		tag := field.Tag.Get("json")
		if tag == "-" {
			continue
		}
		name, _, _ := strings.Cut(tag, ",")
		embedded := field.Type
		if embedded.Kind() == reflect.Pointer {
			embedded = embedded.Elem()
		}
		if field.Anonymous && name == "" && embedded.Kind() == reflect.Struct {
			addFields(fields, embedded, known)
			continue
		}
		if !field.IsExported() {
			continue
		}
		if name == "" {
			name = field.Name
		}
		fields[foldMemberName(name)] = schemaField{name: name, node: schemaOf(field.Type, known)}
	}
}

// foldMemberName over-approximates encoding/json's member matching. Every
// configuration field name is ASCII, and the only non-ASCII runes encoding/json
// folds onto ASCII letters are U+017F (s) and U+212A (k); both fold here too,
// so no spelling that encoding/json would bind to a field is missed. It also
// folds pairs encoding/json keeps apart, such as U+0131 or U+0130 with "i";
// the scan then treats such a name as the field and Decode rejects it.
func foldMemberName(name string) string { return strings.ToLower(strings.ToUpper(name)) }

// uniqueMembers rejects an object that repeats a member name. encoding/json
// keeps the last repetition and matches names case-insensitively, so "url"
// followed by "URL" would silently bind an earlier access key to a different
// relay. core/nodedirectory's uniqueMembers rejects repetitions in signed
// directories for the same reason; it compares names exactly because that
// schema also requires lowercase ASCII names.
//
// The scan follows the document schema. Input that Decode rejects anyway (an
// unknown member, or an object or array where the schema has none) ends the
// scan with ErrConfig, so a reported name always matches a schema field and
// no other input text reaches the message. Opaque subtrees are only checked
// for well-formedness and nesting.
func uniqueMembers(decoder *json.Decoder) error {
	return memberScan{decoder: decoder}.value(0, documentSchema())
}

type memberScan struct{ decoder *json.Decoder }

func (scan memberScan) value(depth int, node *schemaNode) error {
	if depth > maxConfigDepth {
		return ErrConfig
	}
	token, err := scan.decoder.Token()
	if err != nil {
		return err
	}
	switch token {
	case json.Delim('{'):
		err = scan.object(depth, node)
	case json.Delim('['):
		err = scan.array(depth, node)
	default:
		return nil
	}
	if err != nil {
		return err
	}
	_, err = scan.decoder.Token()
	return err
}

func (scan memberScan) array(depth int, node *schemaNode) error {
	element := node.element
	if node.opaque {
		element = node
	} else if element == nil {
		return ErrConfig
	}
	for index := 0; scan.decoder.More(); index++ {
		if err := scan.value(depth+1, element); err != nil {
			return within(err, "["+strconv.Itoa(index)+"]")
		}
	}
	return nil
}

func (scan memberScan) object(depth int, node *schemaNode) error {
	if !node.opaque && node.fields == nil {
		return ErrConfig
	}
	// seen holds the spelling of each field already present; objects have few.
	var seen map[string]string
	for scan.decoder.More() {
		key, err := scan.decoder.Token()
		if err != nil {
			return err
		}
		name, ok := key.(string)
		if !ok {
			return ErrConfig
		}
		if node.opaque {
			if err := scan.value(depth+1, node); err != nil {
				return err
			}
			continue
		}
		field, known := node.fields[foldMemberName(name)]
		if !known {
			return ErrConfig
		}
		if first, repeated := seen[field.name]; repeated {
			return &repeatedMemberError{name: name, first: first}
		}
		if seen == nil {
			seen = make(map[string]string, len(node.fields))
		}
		seen[field.name] = name
		if err := scan.value(depth+1, field.node); err != nil {
			return within(err, field.name)
		}
	}
	return nil
}
