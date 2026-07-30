package browsernetworktopology

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"unicode/utf8"
)

const maximumDocumentBytes = 8 << 20

var ErrNonCanonicalJSON = errors.New("browser network matrix JSON is not canonical")

func decodeCanonicalDocument(encoded []byte, label string, target any, sentinel error) error {
	if len(encoded) == 0 || len(encoded) > maximumDocumentBytes {
		return fmt.Errorf("%w: %s size is outside the contract", sentinel, label)
	}
	if !utf8.Valid(encoded) {
		return fmt.Errorf("%w: %s is not valid UTF-8", sentinel, label)
	}

	decoder := json.NewDecoder(bytes.NewReader(encoded))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(target); err != nil {
		return fmt.Errorf("%w: decode %s: %v", sentinel, label, err)
	}
	var trailing any
	if err := decoder.Decode(&trailing); err != io.EOF {
		return fmt.Errorf("%w: %s contains trailing JSON data", sentinel, label)
	}

	canonical, err := marshalCanonicalDocument(target)
	if err != nil {
		return fmt.Errorf("%w: encode decoded %s: %v", sentinel, label, err)
	}
	if !bytes.Equal(encoded, canonical) {
		// Re-encoding detects duplicate names, alternate escapes, member reordering,
		// and insignificant whitespace without maintaining a second JSON parser.
		return errors.Join(sentinel, ErrNonCanonicalJSON)
	}
	return nil
}

func marshalCanonicalDocument(value any) ([]byte, error) {
	encoded, err := json.Marshal(value)
	if err != nil {
		return nil, err
	}
	// A single terminal LF gives fixture files and generated evidence one exact,
	// editor-friendly byte representation shared with the TypeScript mirror.
	return append(encoded, '\n'), nil
}

func sha256Text(encoded []byte) string {
	digest := sha256.Sum256(encoded)
	return hex.EncodeToString(digest[:])
}

func isCanonicalSHA256(value string) bool {
	if len(value) != sha256.Size*2 {
		return false
	}
	for _, character := range value {
		if (character < '0' || character > '9') && (character < 'a' || character > 'f') {
			return false
		}
	}
	return true
}
