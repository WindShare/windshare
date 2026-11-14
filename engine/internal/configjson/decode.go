// Package configjson decodes configuration objects whose member names are
// case-insensitive, including provider-owned JSON nested in a document.
package configjson

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
)

const maximumDepth = 64

var (
	ErrRepeatedMember = errors.New("repeated configuration member")
	errDocument       = errors.New("invalid configuration JSON")
)

// Decode leaves field selection and custom unmarshaling to encoding/json.
// The additional pass enforces one member policy throughout the document,
// including RawMessage values that will be decoded by another owner.
func Decode(data []byte, target any) error {
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(target); err != nil {
		return err
	}
	var trailing any
	if err := decoder.Decode(&trailing); err != io.EOF {
		return errDocument
	}
	scanner := json.NewDecoder(bytes.NewReader(data))
	// Member checks must not impose float64's range on opaque JSON numbers.
	scanner.UseNumber()
	return memberScan{decoder: scanner}.value(0)
}
