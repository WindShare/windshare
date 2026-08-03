package protocol

import (
	"bytes"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"io"

	"github.com/windshare/windshare/internal/testrun"
)

const MaximumDocumentBytes = testrun.MaximumEventDocumentBytes

func validateDocumentSize(value any, label string) error {
	if _, err := EncodeCanonical(value); err != nil {
		return fmt.Errorf("%s exceeds its canonical document boundary: %w", label, err)
	}
	return nil
}

func EncodeCanonical(value any) ([]byte, error) {
	var output bytes.Buffer
	encoder := json.NewEncoder(&output)
	encoder.SetEscapeHTML(false)
	if err := encoder.Encode(value); err != nil {
		return nil, err
	}
	encoded := output.Bytes()
	if len(encoded) == 0 || encoded[len(encoded)-1] != '\n' {
		return nil, errors.New("JSON encoder omitted record terminator")
	}
	encoded = jsonStringifyCompatible(bytes.Clone(encoded[:len(encoded)-1]))
	if len(encoded) == 0 || len(encoded) > MaximumDocumentBytes {
		return nil, fmt.Errorf("canonical JSON length must be in [1, %d]", MaximumDocumentBytes)
	}
	return encoded, nil
}

func DecodeCanonical[T any](encoded []byte) (T, error) {
	var zero T
	if len(encoded) == 0 || len(encoded) > MaximumDocumentBytes {
		return zero, fmt.Errorf("canonical JSON length must be in [1, %d]", MaximumDocumentBytes)
	}
	decoder := json.NewDecoder(bytes.NewReader(encoded))
	decoder.DisallowUnknownFields()
	var decoded T
	if err := decoder.Decode(&decoded); err != nil {
		return zero, err
	}
	var trailing any
	if err := decoder.Decode(&trailing); !errors.Is(err, io.EOF) {
		return zero, errors.New("canonical JSON contains trailing data")
	}
	canonical, err := EncodeCanonical(decoded)
	if err != nil {
		return zero, err
	}
	if !bytes.Equal(encoded, canonical) {
		return zero, errors.New("JSON document is not canonical")
	}
	return decoded, nil
}

func ReadDocument[T any](reader io.Reader) (T, error) {
	var zero T
	encoded, err := io.ReadAll(io.LimitReader(reader, MaximumDocumentBytes+1))
	if err != nil {
		return zero, err
	}
	return DecodeCanonical[T](encoded)
}

func WriteDocument(writer io.Writer, value any) error {
	encoded, err := EncodeCanonical(value)
	if err != nil {
		return err
	}
	return writeAll(writer, encoded)
}

// ReadLineDocument reads the canonical JSONL form used by pipe transports.
// Requiring exactly one LF keeps the framing contract explicit instead of
// accidentally accepting the broader whitespace rules of encoding/json.
func ReadLineDocument[T any](reader io.Reader) (T, error) {
	var zero T
	encoded, err := io.ReadAll(io.LimitReader(reader, MaximumDocumentBytes+2))
	if err != nil {
		return zero, err
	}
	return DecodeLine[T](encoded)
}

func DecodeLine[T any](encoded []byte) (T, error) {
	var zero T
	if len(encoded) < 2 || len(encoded) > MaximumDocumentBytes+1 || encoded[len(encoded)-1] != '\n' {
		return zero, errors.New("canonical JSON line must contain one document followed by LF")
	}
	return DecodeCanonical[T](encoded[:len(encoded)-1])
}

// WriteLineDocument writes one canonical JSON document followed by one LF.
func WriteLineDocument(writer io.Writer, value any) error {
	encoded, err := EncodeCanonical(value)
	if err != nil {
		return err
	}
	encoded = append(encoded, '\n')
	return writeAll(writer, encoded)
}

func ReadFrame[T any](reader io.Reader) (T, error) {
	var zero T
	header := make([]byte, 4)
	if _, err := io.ReadFull(reader, header); err != nil {
		return zero, err
	}
	length := binary.BigEndian.Uint32(header)
	if length == 0 || length > MaximumDocumentBytes {
		return zero, fmt.Errorf("frame length must be in [1, %d]", MaximumDocumentBytes)
	}
	encoded := make([]byte, int(length))
	if _, err := io.ReadFull(reader, encoded); err != nil {
		return zero, err
	}
	return DecodeCanonical[T](encoded)
}

func WriteFrame(writer io.Writer, value any) error {
	encoded, err := EncodeCanonical(value)
	if err != nil {
		return err
	}
	if len(encoded) == 0 || len(encoded) > MaximumDocumentBytes {
		return fmt.Errorf("frame length must be in [1, %d]", MaximumDocumentBytes)
	}
	header := make([]byte, 4)
	binary.BigEndian.PutUint32(header, uint32(len(encoded)))
	if err := writeAll(writer, header); err != nil {
		return err
	}
	return writeAll(writer, encoded)
}

func jsonStringifyCompatible(encoded []byte) []byte {
	result := make([]byte, 0, len(encoded))
	for index := 0; index < len(encoded); {
		if encoded[index] != '\\' {
			result = append(result, encoded[index])
			index++
			continue
		}
		runEnd := index
		for runEnd < len(encoded) && encoded[runEnd] == '\\' {
			runEnd++
		}
		runLength := runEnd - index
		if runLength%2 == 1 && runEnd+5 <= len(encoded) {
			escape := string(encoded[runEnd : runEnd+5])
			if escape == "u2028" || escape == "u2029" {
				result = append(result, encoded[index:runEnd-1]...)
				if escape == "u2028" {
					result = append(result, "\u2028"...)
				} else {
					result = append(result, "\u2029"...)
				}
				index = runEnd + 5
				continue
			}
		}
		result = append(result, encoded[index:runEnd]...)
		index = runEnd
	}
	return result
}

func writeAll(writer io.Writer, encoded []byte) error {
	for len(encoded) > 0 {
		written, err := writer.Write(encoded)
		if err != nil {
			return err
		}
		if written == 0 {
			return io.ErrShortWrite
		}
		encoded = encoded[written:]
	}
	return nil
}
