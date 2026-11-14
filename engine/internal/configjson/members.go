package configjson

import (
	"encoding/json"
	"errors"
	"strconv"
	"strings"
	"unicode"
)

type memberScan struct{ decoder *json.Decoder }

func (scan memberScan) value(depth int) error {
	if depth > maximumDepth {
		return errDocument
	}
	token, err := scan.decoder.Token()
	if err != nil {
		return err
	}
	switch token {
	case json.Delim('{'):
		err = scan.object(depth)
	case json.Delim('['):
		for index := 0; scan.decoder.More(); index++ {
			if err := scan.value(depth + 1); err != nil {
				return within(err, "["+strconv.Itoa(index)+"]")
			}
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

func (scan memberScan) object(depth int) error {
	seen := make(map[string]string)
	for scan.decoder.More() {
		key, err := scan.decoder.Token()
		if err != nil {
			return err
		}
		name := key.(string) // Decode has already established well-formed JSON.
		folded := foldMemberName(name)
		if first, repeated := seen[folded]; repeated {
			return &repeatedMemberError{name: name, first: first}
		}
		seen[folded] = name
		if err := scan.value(depth + 1); err != nil {
			return within(err, memberPath(folded))
		}
	}
	return nil
}

// SimpleFold cycles encode exactly the equivalence used by encoding/json,
// unlike upper/lower conversion, which also merges Turkish dotted/dotless I.
func foldMemberName(name string) string {
	return strings.Map(func(r rune) rune {
		for {
			next := unicode.SimpleFold(r)
			if next <= r {
				return next
			}
			r = next
		}
	}, name)
}

// Paths are built only on failure; successful scans never copy ancestor paths.
func within(err error, segment string) error {
	if failure, ok := errors.AsType[*repeatedMemberError](err); ok {
		failure.object = append(failure.object, segment)
	}
	return err
}
