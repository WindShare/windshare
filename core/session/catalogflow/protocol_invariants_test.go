package catalogflow

import (
	"bytes"
	"errors"
	"testing"
	"time"

	"github.com/windshare/windshare/core/catalog"
)

func TestCatalogControlDecodersRejectSemanticAndCanonicalityViolations(t *testing.T) {
	fixture := newCatalogObjectFixture(t)
	directory := directoryID(t, 201)
	attempt := scanAttemptID(t, 202)

	for name, encoded := range map[string][]byte{
		"result schema": mustCBOR(t, catalogFlowEnc, map[uint64]any{
			0: catalogControlSchema + 1,
			1: []byte{1},
		}),
		"result field order": marshalIntegerMapInOrder(t, catalogFlowEnc, map[uint64]any{
			0: catalogControlSchema,
			1: []byte{1},
		}, 1, 0),
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := DecodeCatalogResult(encoded); !errors.Is(err, ErrCatalogCodec) {
				t.Fatalf("malformed catalog result error = %v", err)
			}
		})
	}

	validFailureFields := map[uint64]any{
		0: catalogControlSchema,
		1: fixture.share.Bytes(),
		2: directory.Bytes(),
		3: attempt.Bytes(),
		4: uint64(DirectoryCodePermission),
		5: false,
		6: nil,
	}
	zeroShare := cloneIntegerFields(validFailureFields)
	zeroShare[1] = make([]byte, catalog.IdentityBytes)
	retryOverflow := cloneIntegerFields(validFailureFields)
	retryOverflow[4] = uint64(DirectoryCodeTransientIO)
	retryOverflow[5] = true
	retryOverflow[6] = ^uint64(0)
	unknownCode := cloneIntegerFields(validFailureFields)
	unknownCode[4] = uint64(DirectoryCodeCancelled) + 1
	wrongSchema := cloneIntegerFields(validFailureFields)
	wrongSchema[0] = catalogControlSchema + 1

	for name, encoded := range map[string][]byte{
		"wrong schema":   mustCBOR(t, catalogFlowEnc, wrongSchema),
		"zero share":     mustCBOR(t, catalogFlowEnc, zeroShare),
		"retry overflow": mustCBOR(t, catalogFlowEnc, retryOverflow),
		"unknown code":   mustCBOR(t, catalogFlowEnc, unknownCode),
		"field order": marshalIntegerMapInOrder(
			t, catalogFlowEnc, validFailureFields, 6, 5, 4, 3, 2, 1, 0,
		),
	} {
		t.Run("directory failure "+name, func(t *testing.T) {
			if _, err := DecodeDirectoryFailure(encoded); !errors.Is(err, ErrCatalogCodec) {
				t.Fatalf("malformed directory failure error = %v", err)
			}
		})
	}
}

func TestCatalogWireDecodersRejectAuthenticatedSemanticConfusion(t *testing.T) {
	fixture := newCatalogObjectFixture(t)
	directory := directoryID(t, 203)
	generation := generationID(t, 204)

	descriptorFields := map[uint64]any{
		0: wireSchemaVersion,
		1: uint64(catalog.WireVersionV2),
		2: uint64(catalog.SuiteV2),
		3: fixture.share.Bytes(),
		4: directory.Bytes(),
		5: uint64(catalog.MinChunkSize),
		6: uint64(catalog.CapabilityCatalog),
		7: fixture.publicKey,
		8: uint64(1),
		9: catalog.PathPolicyV1,
	}
	badDescriptorNumber := cloneIntegerFields(descriptorFields)
	badDescriptorNumber[1] = ^uint64(0)
	zeroDescriptorShare := cloneIntegerFields(descriptorFields)
	zeroDescriptorShare[3] = make([]byte, catalog.IdentityBytes)
	zeroDescriptorRoot := cloneIntegerFields(descriptorFields)
	zeroDescriptorRoot[4] = make([]byte, catalog.IdentityBytes)
	reservedCapability := cloneIntegerFields(descriptorFields)
	reservedCapability[6] = uint64(catalog.CapabilityShareInstanceRefreshReserved)

	for name, test := range map[string]struct {
		encoded []byte
		want    error
	}{
		"overflowed version":  {mustCBOR(t, catalogWireEnc, badDescriptorNumber), ErrWireObject},
		"zero share":          {mustCBOR(t, catalogWireEnc, zeroDescriptorShare), ErrWireObject},
		"zero root":           {mustCBOR(t, catalogWireEnc, zeroDescriptorRoot), ErrWireObject},
		"reserved capability": {mustCBOR(t, catalogWireEnc, reservedCapability), ErrWireObject},
		"field order": {
			marshalIntegerMapInOrder(t, catalogWireEnc, descriptorFields, 9, 8, 7, 6, 5, 4, 3, 2, 1, 0),
			ErrNonCanonicalWire,
		},
	} {
		t.Run("descriptor "+name, func(t *testing.T) {
			if _, err := decodeShareDescriptor(test.encoded); !errors.Is(err, test.want) {
				t.Fatalf("descriptor error = %v, want %v", err, test.want)
			}
		})
	}

	pageFields := map[uint64]any{
		0: wireSchemaVersion,
		1: fixture.share.Bytes(),
		2: directory.Bytes(),
		3: generation.Bytes(),
		4: uint64(0),
		5: true,
		6: make([]byte, catalog.PageCommitmentBytes),
		7: []any{},
		8: uint64(0),
	}
	wrongTerminal := cloneIntegerFields(pageFields)
	wrongTerminal[5] = "true"
	zeroGeneration := cloneIntegerFields(pageFields)
	zeroGeneration[3] = make([]byte, catalog.IdentityBytes)
	wrongEntries := cloneIntegerFields(pageFields)
	wrongEntries[7] = "entries"
	invalidSequence := cloneIntegerFields(pageFields)
	invalidSequence[4] = uint64(1)
	invalidSequence[6] = bytes.Repeat([]byte{1}, catalog.PageCommitmentBytes)

	for name, test := range map[string]struct {
		encoded []byte
		want    error
	}{
		"wrong terminal type": {mustCBOR(t, catalogWireEnc, wrongTerminal), ErrWireObject},
		"zero generation":     {mustCBOR(t, catalogWireEnc, zeroGeneration), ErrWireObject},
		"entries not array":   {mustCBOR(t, catalogWireEnc, wrongEntries), ErrWireObject},
		"invalid sequence":    {mustCBOR(t, catalogWireEnc, invalidSequence), ErrWireObject},
		"field order": {
			marshalIntegerMapInOrder(t, catalogWireEnc, pageFields, 8, 7, 6, 5, 4, 3, 2, 1, 0),
			ErrNonCanonicalWire,
		},
	} {
		t.Run("page "+name, func(t *testing.T) {
			if _, err := decodeCatalogPage(test.encoded, testCommitter{}); !errors.Is(err, test.want) {
				t.Fatalf("page error = %v, want %v", err, test.want)
			}
		})
	}
}

func TestCatalogEntryDecoderRejectsAmbiguousIdentityAndTimeFields(t *testing.T) {
	directory := directoryID(t, 205)
	file := fileID(t, 206)
	zeroIdentity := make([]byte, catalog.IdentityBytes)

	for name, encoded := range map[string][]byte{
		"wrong arity": mustRawCBOR(t, []any{
			uint64(catalog.NodeKindDirectory), directory.Bytes(), "dir", nil, nil, uint64(0),
		}),
		"identity type": mustRawCBOR(t, []any{
			uint64(catalog.NodeKindDirectory), "identity", "dir", nil, nil, uint64(0), uint64(0),
		}),
		"modified type": mustRawCBOR(t, []any{
			uint64(catalog.NodeKindDirectory), directory.Bytes(), "dir", nil, "seconds", uint64(0), uint64(0),
		}),
		"zero directory": mustRawCBOR(t, []any{
			uint64(catalog.NodeKindDirectory), zeroIdentity, "dir", nil, nil, uint64(0), uint64(0),
		}),
		"zero file": mustRawCBOR(t, []any{
			uint64(catalog.NodeKindFile), zeroIdentity, "file", uint64(1), nil, uint64(0), uint64(0),
		}),
		"file size type": mustRawCBOR(t, []any{
			uint64(catalog.NodeKindFile), file.Bytes(), "file", "one", nil, uint64(0), uint64(0),
		}),
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := decodeWireEntry(encoded); err == nil {
				t.Fatalf("entry error = %v", err)
			}
		})
	}

	if _, err := decodeWireModified([]byte{0xff}, mustRawCBOR(t, uint64(0)), mustRawCBOR(t, uint64(0))); !errors.Is(err, ErrWireObject) {
		t.Fatalf("invalid signed timestamp error = %v", err)
	}
	if _, err := decodeWireModified(
		mustRawCBOR(t, int64(0)),
		mustRawCBOR(t, uint64(^uint32(0))+1),
		mustRawCBOR(t, uint64(catalog.TimePrecisionNanoseconds)),
	); !errors.Is(err, ErrWireObject) {
		t.Fatalf("overflowed timestamp error = %v", err)
	}
}

func TestListRequestDecoderRejectsTypedAndNonCanonicalAuthority(t *testing.T) {
	directory := directoryID(t, 207)

	for name, encoded := range map[string][]byte{
		"directory type":           mustCBOR(t, requestEncMode, []any{uint64(1), nil, uint64(0)}),
		"generation type":          mustCBOR(t, requestEncMode, []any{directory.Bytes(), "generation", uint64(0)}),
		"page type":                mustCBOR(t, requestEncMode, []any{directory.Bytes(), nil, "page"}),
		"later without generation": mustCBOR(t, requestEncMode, []any{directory.Bytes(), nil, uint64(1)}),
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := DecodeListRequest(encoded); !errors.Is(err, ErrInvalidRequest) {
				t.Fatalf("request error = %v", err)
			}
		})
	}

	// A non-minimal integer is semantically zero but must not survive the
	// deterministic encoding boundary used as the signed request authority.
	nonCanonical := append([]byte{0x83, 0x50}, directory.Bytes()...)
	nonCanonical = append(nonCanonical, 0xf6, 0x18, 0x00)
	if _, err := DecodeListRequest(nonCanonical); err == nil {
		t.Fatal("noncanonical request was accepted")
	}
}

func TestAssemblerRejectsFailureAndPageTransitionsOutsideOneGeneration(t *testing.T) {
	share := shareInstance(t, 208)
	directory := directoryID(t, 209)
	snapshot := twoPageSnapshot(t, share, directory, 210, "first", "second")
	pages := snapshot.Pages()

	t.Run("failure after page", func(t *testing.T) {
		assembler, _ := NewAssembler(share, directory, 4)
		if _, err := assembler.Accept(VerifiedPage(pages[0])); err != nil {
			t.Fatal(err)
		}
		failure := mustDirectoryFailure(t, share, directory, 211, DirectoryCodePermission, false)
		if _, err := assembler.Accept(VerifiedFailure(failure)); !errors.Is(err, ErrPageConflict) {
			t.Fatalf("partial generation failure = %v", err)
		}
	})

	t.Run("invalid failure", func(t *testing.T) {
		assembler, _ := NewAssembler(share, directory, 4)
		invalid := DirectoryFailure{
			ShareInstance: share,
			DirectoryID:   directory,
			AttemptID:     scanAttemptID(t, 212),
			Code:          DirectoryCodeTransientIO,
			Retryable:     true,
			RetryAfter:    time.Millisecond,
		}
		if _, err := assembler.Accept(VerifiedFailure(invalid)); err == nil {
			t.Fatal("invalid authenticated failure was accepted")
		}
	})

	t.Run("generation switch", func(t *testing.T) {
		assembler, _ := NewAssembler(share, directory, 4)
		if _, err := assembler.Accept(VerifiedPage(pages[0])); err != nil {
			t.Fatal(err)
		}
		other := twoPageSnapshot(t, share, directory, 220, "other-first", "other-second").Pages()[1]
		if _, err := assembler.Accept(VerifiedPage(other)); !errors.Is(err, ErrObjectIdentity) {
			t.Fatalf("generation switch = %v", err)
		}
	})

	t.Run("gap after first page", func(t *testing.T) {
		assembler, _ := NewAssembler(share, directory, 4)
		if _, err := assembler.Accept(VerifiedPage(pages[0])); err != nil {
			t.Fatal(err)
		}
		gap := assemblyPage(t, share, directory, snapshot.Generation(), 2, pages[0].Commitment(), 213)
		if _, err := assembler.Accept(VerifiedPage(gap)); !errors.Is(err, ErrPageGap) {
			t.Fatalf("nonconsecutive page = %v", err)
		}
	})

	t.Run("wrong predecessor", func(t *testing.T) {
		assembler, _ := NewAssembler(share, directory, 4)
		if _, err := assembler.Accept(VerifiedPage(pages[0])); err != nil {
			t.Fatal(err)
		}
		wrongPrevious, err := catalog.NewPageCommitment(bytes.Repeat([]byte{0xee}, catalog.PageCommitmentBytes))
		if err != nil {
			t.Fatal(err)
		}
		page := assemblyPage(t, share, directory, snapshot.Generation(), 1, wrongPrevious, 214)
		if _, err := assembler.Accept(VerifiedPage(page)); !errors.Is(err, ErrPageConflict) {
			t.Fatalf("wrong predecessor = %v", err)
		}
	})
}

func marshalIntegerMapInOrder(
	t *testing.T,
	mode cborMarshaler,
	fields map[uint64]any,
	order ...uint64,
) []byte {
	t.Helper()
	if len(fields) != len(order) || len(fields) >= 24 {
		t.Fatal("test map order must cover every small integer field exactly once")
	}
	encoded := []byte{0xa0 | byte(len(fields))}
	seen := make(map[uint64]struct{}, len(order))
	for _, key := range order {
		value, ok := fields[key]
		if !ok {
			t.Fatalf("test map order references missing field %d", key)
		}
		if _, duplicate := seen[key]; duplicate {
			t.Fatalf("test map order repeats field %d", key)
		}
		seen[key] = struct{}{}
		encoded = append(encoded, mustCBOR(t, mode, key)...)
		encoded = append(encoded, mustCBOR(t, mode, value)...)
	}
	return encoded
}

func cloneIntegerFields(source map[uint64]any) map[uint64]any {
	cloned := make(map[uint64]any, len(source))
	for key, value := range source {
		cloned[key] = value
	}
	return cloned
}

func assemblyPage(
	t *testing.T,
	share catalog.ShareInstance,
	directory catalog.DirectoryID,
	generation catalog.DirectoryGeneration,
	index uint32,
	previous catalog.PageCommitment,
	identity byte,
) catalog.CatalogPage {
	t.Helper()
	entry, err := catalog.NewFileEntry(fileID(t, identity), "entry", 1, catalog.ModifiedTime{})
	if err != nil {
		t.Fatal(err)
	}
	page, err := catalog.NewCatalogPage(catalog.CatalogPageSpec{
		ShareInstance: share,
		DirectoryID:   directory,
		Generation:    generation,
		PageIndex:     index,
		Previous:      previous,
		Entries:       []catalog.Entry{entry},
		Terminal:      true,
	}, testCommitter{})
	if err != nil {
		t.Fatal(err)
	}
	return page
}
