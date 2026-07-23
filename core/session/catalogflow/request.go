package catalogflow

import (
	"bytes"
	"errors"
	"fmt"
	"math"

	"github.com/fxamacker/cbor/v2"
	"github.com/windshare/windshare/core/catalog"
)

const MaxListRequestBytes = 40

type ListRequest struct {
	directory  catalog.DirectoryID
	generation *catalog.DirectoryGeneration
	pageIndex  uint32
}

func NewListRequest(directory catalog.DirectoryID, generation *catalog.DirectoryGeneration, pageIndex uint32) (ListRequest, error) {
	if directory.IsZero() {
		return ListRequest{}, fmt.Errorf("%w: directory identity is zero", ErrInvalidRequest)
	}
	if pageIndex > 0 && generation == nil {
		return ListRequest{}, fmt.Errorf("%w: later pages require a generation", ErrInvalidRequest)
	}
	var generationCopy *catalog.DirectoryGeneration
	if generation != nil {
		if generation.IsZero() {
			return ListRequest{}, fmt.Errorf("%w: generation identity is zero", ErrInvalidRequest)
		}
		copy := *generation
		generationCopy = &copy
	}
	return ListRequest{directory: directory, generation: generationCopy, pageIndex: pageIndex}, nil
}

func (r ListRequest) DirectoryID() catalog.DirectoryID { return r.directory }
func (r ListRequest) PageIndex() uint32                { return r.pageIndex }
func (r ListRequest) Generation() (catalog.DirectoryGeneration, bool) {
	if r.generation == nil {
		return catalog.DirectoryGeneration{}, false
	}
	return *r.generation, true
}

var requestEncMode = func() cbor.EncMode {
	options := cbor.CoreDetEncOptions()
	mode, err := options.EncMode()
	if err != nil {
		panic(err)
	}
	return mode
}()

var requestDecMode = func() cbor.DecMode {
	mode, err := cbor.DecOptions{
		DupMapKey:        cbor.DupMapKeyEnforcedAPF,
		IndefLength:      cbor.IndefLengthForbidden,
		TagsMd:           cbor.TagsForbidden,
		MaxArrayElements: 16,
	}.DecMode()
	if err != nil {
		panic(err)
	}
	return mode
}()

func EncodeListRequest(request ListRequest) ([]byte, error) {
	if request.directory.IsZero() || (request.pageIndex > 0 && request.generation == nil) {
		return nil, ErrInvalidRequest
	}
	var generation any
	if request.generation != nil {
		generation = request.generation.Bytes()
	}
	encoded, err := requestEncMode.Marshal([]any{request.directory.Bytes(), generation, uint64(request.pageIndex)})
	if err != nil {
		return nil, fmt.Errorf("encode catalog list request: %w", err)
	}
	return encoded, nil
}

func DecodeListRequest(encoded []byte) (ListRequest, error) {
	if len(encoded) == 0 || len(encoded) > MaxListRequestBytes {
		return ListRequest{}, ErrInvalidRequest
	}
	var fields []cbor.RawMessage
	if err := requestDecMode.Unmarshal(encoded, &fields); err != nil || len(fields) != 3 {
		return ListRequest{}, fmt.Errorf("%w: malformed canonical array", ErrInvalidRequest)
	}
	var directoryBytes []byte
	if err := requestDecMode.Unmarshal(fields[0], &directoryBytes); err != nil {
		return ListRequest{}, fmt.Errorf("%w: directory identity: %w", ErrInvalidRequest, err)
	}
	directory, err := catalog.DirectoryIDFromBytes(directoryBytes)
	if err != nil {
		return ListRequest{}, fmt.Errorf("%w: %w", ErrInvalidRequest, err)
	}
	var generation *catalog.DirectoryGeneration
	if !bytes.Equal(fields[1], []byte{0xf6}) {
		var generationBytes []byte
		if err := requestDecMode.Unmarshal(fields[1], &generationBytes); err != nil {
			return ListRequest{}, fmt.Errorf("%w: generation identity: %w", ErrInvalidRequest, err)
		}
		parsed, parseErr := catalog.DirectoryGenerationFromBytes(generationBytes)
		if parseErr != nil {
			return ListRequest{}, fmt.Errorf("%w: %w", ErrInvalidRequest, parseErr)
		}
		generation = &parsed
	}
	var page uint64
	if err := requestDecMode.Unmarshal(fields[2], &page); err != nil || page > math.MaxUint32 {
		return ListRequest{}, fmt.Errorf("%w: page index", ErrInvalidRequest)
	}
	request, err := NewListRequest(directory, generation, uint32(page))
	if err != nil {
		return ListRequest{}, err
	}
	canonical, err := EncodeListRequest(request)
	if err != nil {
		return ListRequest{}, err
	}
	if !bytes.Equal(canonical, encoded) {
		return ListRequest{}, errors.New("catalog list request is not deterministic CBOR")
	}
	return request, nil
}
