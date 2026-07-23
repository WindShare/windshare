package catalogflow

import (
	"bytes"
	"errors"
	"math"
	"time"

	"github.com/fxamacker/cbor/v2"

	"github.com/windshare/windshare/core/catalog"
)

const catalogControlSchema = uint64(1)

var ErrCatalogCodec = errors.New("catalog flow object is malformed")

var catalogFlowEnc = func() cbor.EncMode {
	mode, err := cbor.CoreDetEncOptions().EncMode()
	if err != nil {
		panic(err)
	}
	return mode
}()

var catalogFlowDec = func() cbor.DecMode {
	mode, err := cbor.DecOptions{
		DupMapKey:        cbor.DupMapKeyEnforcedAPF,
		IndefLength:      cbor.IndefLengthForbidden,
		TagsMd:           cbor.TagsForbidden,
		MaxNestedLevels:  8,
		MaxArrayElements: 16,
		MaxMapPairs:      16,
	}.DecMode()
	if err != nil {
		panic(err)
	}
	return mode
}()

func EncodeCatalogResult(object []byte) ([]byte, error) {
	if len(object) == 0 || len(object) > catalog.MaxCatalogPageObjectBytes {
		return nil, ErrCatalogCodec
	}
	return catalogFlowEnc.Marshal(map[uint64]any{0: catalogControlSchema, 1: bytes.Clone(object)})
}

func DecodeCatalogResult(encoded []byte) ([]byte, error) {
	fields, err := decodeCatalogMap(encoded, 2)
	if err != nil {
		return nil, err
	}
	schema, schemaErr := catalogUint(fields[0])
	var object []byte
	objectErr := catalogFlowDec.Unmarshal(fields[1], &object)
	if schemaErr != nil || schema != catalogControlSchema || objectErr != nil ||
		len(object) == 0 || len(object) > catalog.MaxCatalogPageObjectBytes {
		return nil, ErrCatalogCodec
	}
	canonical, _ := EncodeCatalogResult(object)
	if !bytes.Equal(canonical, encoded) {
		return nil, ErrCatalogCodec
	}
	return bytes.Clone(object), nil
}

func EncodeDirectoryFailure(failure DirectoryFailure) ([]byte, error) {
	validated, err := NewDirectoryFailure(failure)
	if err != nil {
		return nil, err
	}
	var retry any
	if validated.Retryable {
		retry = uint64(validated.RetryAfter / time.Millisecond)
	}
	return catalogFlowEnc.Marshal(map[uint64]any{
		0: catalogControlSchema,
		1: validated.ShareInstance.Bytes(),
		2: validated.DirectoryID.Bytes(),
		3: validated.AttemptID.Bytes(),
		4: uint64(validated.Code),
		5: validated.Retryable,
		6: retry,
	})
}

func DecodeDirectoryFailure(encoded []byte) (DirectoryFailure, error) {
	fields, err := decodeCatalogMap(encoded, 7)
	if err != nil {
		return DirectoryFailure{}, err
	}
	schema, schemaErr := catalogUint(fields[0])
	shareBytes, shareErr := catalogBytes(fields[1], catalog.IdentityBytes)
	directoryBytes, directoryErr := catalogBytes(fields[2], catalog.IdentityBytes)
	attemptBytes, attemptErr := catalogBytes(fields[3], catalog.IdentityBytes)
	code, codeErr := catalogUint(fields[4])
	var retryable bool
	retryableErr := catalogFlowDec.Unmarshal(fields[5], &retryable)
	if schemaErr != nil || schema != catalogControlSchema || shareErr != nil || directoryErr != nil ||
		attemptErr != nil || codeErr != nil || code > math.MaxUint16 || retryableErr != nil {
		return DirectoryFailure{}, ErrCatalogCodec
	}
	share, shareErr := catalog.ShareInstanceFromBytes(shareBytes)
	directory, directoryErr := catalog.DirectoryIDFromBytes(directoryBytes)
	attempt, attemptErr := catalog.ScanAttemptIDFromBytes(attemptBytes)
	if shareErr != nil || directoryErr != nil || attemptErr != nil {
		return DirectoryFailure{}, ErrCatalogCodec
	}
	var retryAfter time.Duration
	if retryable {
		milliseconds, err := catalogUint(fields[6])
		if err != nil || milliseconds > uint64(math.MaxInt64/int64(time.Millisecond)) {
			return DirectoryFailure{}, ErrCatalogCodec
		}
		retryAfter = time.Duration(milliseconds) * time.Millisecond
	} else if !bytes.Equal(fields[6], []byte{0xf6}) {
		return DirectoryFailure{}, ErrCatalogCodec
	}
	failure, err := NewDirectoryFailure(DirectoryFailure{
		ShareInstance: share, DirectoryID: directory, AttemptID: attempt,
		Code: uint16(code), Retryable: retryable, RetryAfter: retryAfter,
	})
	if err != nil {
		return DirectoryFailure{}, errors.Join(ErrCatalogCodec, err)
	}
	canonical, _ := EncodeDirectoryFailure(failure)
	if !bytes.Equal(canonical, encoded) {
		return DirectoryFailure{}, ErrCatalogCodec
	}
	return failure, nil
}

func decodeCatalogMap(encoded []byte, exact int) (map[uint64]cbor.RawMessage, error) {
	var fields map[uint64]cbor.RawMessage
	if err := catalogFlowDec.Unmarshal(encoded, &fields); err != nil || len(fields) != exact {
		return nil, ErrCatalogCodec
	}
	for index := range exact {
		if fields[uint64(index)] == nil {
			return nil, ErrCatalogCodec
		}
	}
	return fields, nil
}

func catalogUint(encoded []byte) (uint64, error) {
	var value uint64
	if err := catalogFlowDec.Unmarshal(encoded, &value); err != nil {
		return 0, ErrCatalogCodec
	}
	return value, nil
}

func catalogBytes(encoded []byte, exact int) ([]byte, error) {
	var value []byte
	if err := catalogFlowDec.Unmarshal(encoded, &value); err != nil || len(value) != exact {
		return nil, ErrCatalogCodec
	}
	return bytes.Clone(value), nil
}
