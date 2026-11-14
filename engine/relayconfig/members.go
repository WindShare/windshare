package relayconfig

import (
	"encoding/json"
	"fmt"
	"strings"
)

// ErrRepeatedMember reports an object that names a member twice. It is
// distinct from other ErrConfig causes because jq and python -m json.tool keep
// one repetition silently, so a user checking the file with them would not see
// the problem unless the error names the member.
var ErrRepeatedMember = fmt.Errorf("%w; an object member appears more than once", ErrConfig)

// repeatedMemberError names only members and their object, never values: the
// repeated member is usually a URL or an access key.
type repeatedMemberError struct{ name, first, object string }

func (failure *repeatedMemberError) Error() string {
	object := failure.object
	if object == "" {
		object = "the top-level object"
	}
	message := fmt.Sprintf("%s; member %q appears more than once in %s", ErrConfig, failure.name, object)
	if failure.first != failure.name {
		message += fmt.Sprintf(" (also written as %q; member names match case-insensitively)", failure.first)
	}
	return message
}

func (failure *repeatedMemberError) Unwrap() error { return ErrRepeatedMember }

// memberScope locates a value in the configuration schema, so the scan can
// treat exactly one subtree as opaque.
type memberScope uint8

const (
	typedScope memberScope = iota
	rootScope
	turnScope
	turnAccountsScope
	turnAccountScope
	// opaqueScope is turn.accounts[].credentials. It is a json.RawMessage that
	// older versions wrote back verbatim, repetitions included, so rejecting
	// them here would stop an upgraded device from loading its own file.
	// Interpreting the credentials, including what a repetition inside them
	// means, is turnruntime's responsibility; this scan only requires the
	// subtree to be well-formed and within the nesting limit.
	opaqueScope
)

// The names follow the document and turn.ManualSettings JSON tags.
// strings.EqualFold is the comparison encoding/json uses to bind a member to
// a field, so the opaque subtree is exactly the one decoded as RawMessage.
func (scope memberScope) member(name string) memberScope {
	switch {
	case scope == opaqueScope:
		return opaqueScope
	case scope == rootScope && strings.EqualFold(name, "turn"):
		return turnScope
	case scope == turnScope && strings.EqualFold(name, "accounts"):
		return turnAccountsScope
	case scope == turnAccountScope && strings.EqualFold(name, "credentials"):
		return opaqueScope
	}
	return typedScope
}

func (scope memberScope) element() memberScope {
	switch scope {
	case opaqueScope:
		return opaqueScope
	case turnAccountsScope:
		return turnAccountScope
	}
	return typedScope
}

// foldMemberName over-approximates encoding/json's member matching. Every
// configuration field name is ASCII, and the only non-ASCII runes encoding/json
// folds onto ASCII letters are U+017F (s) and U+212A (k); both fold here too,
// so no spelling that encoding/json would merge into one field is missed. It
// also folds pairs encoding/json keeps apart, such as U+0131 or U+0130 with
// "i", but such names are unknown fields, which Decode rejects anyway.
func foldMemberName(name string) string { return strings.ToLower(strings.ToUpper(name)) }

// uniqueMembers rejects an object that repeats a member name. encoding/json
// keeps the last repetition and matches names case-insensitively, so "url"
// followed by "URL" would silently bind an earlier access key to a different
// relay. core/nodedirectory's uniqueMembers rejects repetitions in signed
// directories for the same reason; it compares names exactly because that
// schema also requires lowercase ASCII names.
func uniqueMembers(decoder *json.Decoder) error {
	return memberScan{decoder: decoder}.value(0, rootScope, "")
}

type memberScan struct{ decoder *json.Decoder }

func (scan memberScan) value(depth int, scope memberScope, path string) error {
	if depth > maxConfigDepth {
		return ErrConfig
	}
	token, err := scan.decoder.Token()
	if err != nil {
		return err
	}
	switch token {
	case json.Delim('{'):
		err = scan.object(depth, scope, path)
	case json.Delim('['):
		for index := 0; err == nil && scan.decoder.More(); index++ {
			err = scan.value(depth+1, scope.element(), fmt.Sprintf("%s[%d]", path, index))
		}
	default:
		return nil
	}
	if err != nil {
		return err
	}
	_, err = scan.decoder.Token()
	return err
}

func (scan memberScan) object(depth int, scope memberScope, path string) error {
	seen := map[string]string{}
	for scan.decoder.More() {
		key, err := scan.decoder.Token()
		if err != nil {
			return err
		}
		name, ok := key.(string)
		if !ok {
			return ErrConfig
		}
		if scope != opaqueScope {
			folded := foldMemberName(name)
			if first, repeated := seen[folded]; repeated {
				return &repeatedMemberError{name: name, first: first, object: path}
			}
			seen[folded] = name
		}
		if err := scan.value(depth+1, scope.member(name), memberPath(path, name)); err != nil {
			return err
		}
	}
	return nil
}

func memberPath(object, name string) string {
	if object == "" {
		return name
	}
	return object + "." + name
}
