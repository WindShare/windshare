package browsermatrixfixture

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
)

func canonicalJSONLine(value any) ([]byte, error) {
	var buffer bytes.Buffer
	encoder := json.NewEncoder(&buffer)
	encoder.SetEscapeHTML(false)
	err := encoder.Encode(value)
	if err != nil {
		return nil, err
	}
	return unescapeJavaScriptLineSeparators(buffer.Bytes()), nil
}

func marshalCanonicalObject(value any) ([]byte, error) {
	document, err := canonicalJSONLine(value)
	if err != nil {
		return nil, err
	}
	return document[:len(document)-1], nil
}

func unescapeJavaScriptLineSeparators(document []byte) []byte {
	result := make([]byte, 0, len(document))
	for index := 0; index < len(document); {
		if document[index] != '\\' {
			result = append(result, document[index])
			index++
			continue
		}
		runEnd := index
		for runEnd < len(document) && document[runEnd] == '\\' {
			runEnd++
		}
		runLength := runEnd - index
		if runLength%2 == 1 && runEnd+5 <= len(document) {
			escape := string(document[runEnd : runEnd+5])
			if escape == "u2028" || escape == "u2029" {
				result = append(result, document[index:runEnd-1]...)
				if escape == "u2028" {
					result = append(result, []byte("\u2028")...)
				} else {
					result = append(result, []byte("\u2029")...)
				}
				index = runEnd + 5
				continue
			}
		}
		result = append(result, document[index:runEnd]...)
		index = runEnd
	}
	return result
}

func decodeExactJSON(document []byte, destination any) error {
	decoder := json.NewDecoder(bytes.NewReader(document))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(destination); err != nil {
		return err
	}
	var trailing json.RawMessage
	if err := decoder.Decode(&trailing); !errors.Is(err, io.EOF) {
		return errors.New("JSON document has trailing data")
	}
	return nil
}
