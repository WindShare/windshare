package catalogflow

import (
	"bytes"
	"errors"
	"math"

	"github.com/fxamacker/cbor/v2"
	"github.com/windshare/windshare/core/catalog"
)

const wireSchemaVersion = uint64(1)

var (
	ErrWireObject       = errors.New("catalog wire object is invalid")
	ErrNonCanonicalWire = errors.New("catalog wire object is not canonical CBOR")
)

var catalogWireEnc = func() cbor.EncMode {
	mode, err := cbor.CoreDetEncOptions().EncMode()
	if err != nil {
		panic(err)
	}
	return mode
}()

var catalogWireDec = func() cbor.DecMode {
	mode, err := cbor.DecOptions{
		DupMapKey:        cbor.DupMapKeyEnforcedAPF,
		IndefLength:      cbor.IndefLengthForbidden,
		TagsMd:           cbor.TagsForbidden,
		MaxNestedLevels:  8,
		MaxArrayElements: catalog.MaxCatalogPageEntries,
		MaxMapPairs:      16,
	}.DecMode()
	if err != nil {
		panic(err)
	}
	return mode
}()

func encodeShareDescriptor(descriptor catalog.ShareDescriptor) ([]byte, error) {
	if err := validateDescriptorValue(descriptor); err != nil {
		return nil, err
	}
	return catalogWireEnc.Marshal(map[uint64]any{
		0: wireSchemaVersion,
		1: uint64(descriptor.WireVersion()),
		2: uint64(descriptor.Suite()),
		3: descriptor.ShareInstance().Bytes(),
		4: descriptor.SyntheticRoot().Bytes(),
		5: uint64(descriptor.ChunkSize()),
		6: uint64(descriptor.Capabilities()),
		7: descriptor.SenderPublicKey(),
		8: descriptor.CreatedAtSeconds(),
		9: descriptor.PathPolicy(),
	})
}

// DecodeShareDescriptor constructs receiver authority from authenticated wire
// bytes. It deliberately does not manufacture CommittedRoot, which remains a
// sender-local registration capability.
func decodeShareDescriptor(encoded []byte) (catalog.ShareDescriptor, error) {
	fields, err := decodeWireMap(encoded, 10)
	if err != nil {
		return catalog.ShareDescriptor{}, err
	}
	if schema, err := wireUint(fields[0]); err != nil || schema != wireSchemaVersion {
		return catalog.ShareDescriptor{}, ErrWireObject
	}
	wireVersion, wireErr := wireUint(fields[1])
	suite, suiteErr := wireUint(fields[2])
	shareBytes, shareErr := wireBytes(fields[3], catalog.IdentityBytes)
	rootBytes, rootErr := wireBytes(fields[4], catalog.IdentityBytes)
	chunkSize, chunkErr := wireUint(fields[5])
	capabilities, capabilityErr := wireUint(fields[6])
	senderKey, keyErr := wireBytes(fields[7], catalog.SenderPublicKeySize)
	created, createdErr := wireUint(fields[8])
	pathPolicy, pathErr := wireText(fields[9])
	if wireErr != nil || suiteErr != nil || shareErr != nil || rootErr != nil || chunkErr != nil ||
		capabilityErr != nil || keyErr != nil || createdErr != nil || pathErr != nil ||
		wireVersion > math.MaxUint32 || suite > math.MaxUint32 || chunkSize > math.MaxUint32 {
		return catalog.ShareDescriptor{}, ErrWireObject
	}
	share, err := catalog.ShareInstanceFromBytes(shareBytes)
	if err != nil {
		return catalog.ShareDescriptor{}, ErrWireObject
	}
	root, err := catalog.DirectoryIDFromBytes(rootBytes)
	if err != nil {
		return catalog.ShareDescriptor{}, ErrWireObject
	}
	descriptor, err := catalog.NewReceivedShareDescriptor(catalog.ReceivedDescriptorSpec{
		WireVersion: uint32(wireVersion), Suite: uint32(suite), ShareInstance: share,
		SyntheticRoot: root, ChunkSize: uint32(chunkSize), Capabilities: catalog.Capability(capabilities),
		SenderPublicKey: senderKey, CreatedAtSeconds: created, PathPolicy: pathPolicy,
	})
	if err != nil {
		return catalog.ShareDescriptor{}, errors.Join(ErrWireObject, err)
	}
	canonical, _ := encodeShareDescriptor(descriptor)
	if !bytes.Equal(canonical, encoded) {
		return catalog.ShareDescriptor{}, ErrNonCanonicalWire
	}
	return descriptor, nil
}

func validateDescriptorValue(descriptor catalog.ShareDescriptor) error {
	if descriptor.WireVersion() != catalog.WireVersionV2 || descriptor.Suite() != catalog.SuiteV2 ||
		descriptor.ShareInstance().IsZero() || descriptor.SyntheticRoot().IsZero() ||
		descriptor.ChunkSize() < catalog.MinChunkSize || descriptor.ChunkSize() > catalog.MaxChunkSize ||
		descriptor.ChunkSize()&(descriptor.ChunkSize()-1) != 0 ||
		len(descriptor.SenderPublicKey()) != catalog.SenderPublicKeySize ||
		descriptor.CreatedAtSeconds() > catalog.MaxSafeInteger || descriptor.PathPolicy() != catalog.PathPolicyV1 {
		return ErrWireObject
	}
	return nil
}

func encodeCatalogPage(page catalog.CatalogPage) ([]byte, error) {
	if page.Commitment().IsZero() {
		return nil, ErrWireObject
	}
	return encodeCatalogPageFields(catalog.PageCommitInput{
		ShareInstance: page.ShareInstance(), DirectoryID: page.DirectoryID(), Generation: page.Generation(),
		PageIndex: page.PageIndex(), Previous: page.Previous(), Entries: page.Entries(),
		Terminal: page.Terminal(), OmittedCount: page.OmittedCount(),
	})
}

// EncodeCatalogPageInput lets a PageCommitter seal the exact page bytes before
// NewCatalogPage stores the resulting full-object digest as its commitment.
func encodeCatalogPageInput(input catalog.PageCommitInput) ([]byte, error) {
	return encodeCatalogPageFields(input)
}

func encodeCatalogPageFields(input catalog.PageCommitInput) ([]byte, error) {
	entries := make([]any, len(input.Entries))
	for index, entry := range input.Entries {
		encoded, err := encodeWireEntry(entry)
		if err != nil {
			return nil, err
		}
		entries[index] = encoded
	}
	return catalogWireEnc.Marshal(map[uint64]any{
		0: wireSchemaVersion,
		1: input.ShareInstance.Bytes(),
		2: input.DirectoryID.Bytes(),
		3: input.Generation.Bytes(),
		4: uint64(input.PageIndex),
		5: input.Terminal,
		6: input.Previous.Bytes(),
		7: entries,
		8: input.OmittedCount,
	})
}

func decodeCatalogPage(encoded []byte, committer catalog.PageCommitter) (catalog.CatalogPage, error) {
	fields, err := decodeWireMap(encoded, 9)
	if err != nil || committer == nil {
		return catalog.CatalogPage{}, ErrWireObject
	}
	if schema, schemaErr := wireUint(fields[0]); schemaErr != nil || schema != wireSchemaVersion {
		return catalog.CatalogPage{}, ErrWireObject
	}
	shareBytes, shareErr := wireBytes(fields[1], catalog.IdentityBytes)
	directoryBytes, directoryErr := wireBytes(fields[2], catalog.IdentityBytes)
	generationBytes, generationErr := wireBytes(fields[3], catalog.IdentityBytes)
	pageIndex, pageErr := wireUint(fields[4])
	terminal, terminalErr := wireBool(fields[5])
	previousBytes, previousErr := wireBytes(fields[6], catalog.PageCommitmentBytes)
	omitted, omittedErr := wireUint(fields[8])
	if shareErr != nil || directoryErr != nil || generationErr != nil || pageErr != nil ||
		terminalErr != nil || previousErr != nil || omittedErr != nil || pageIndex > math.MaxUint32 {
		return catalog.CatalogPage{}, ErrWireObject
	}
	share, shareErr := catalog.ShareInstanceFromBytes(shareBytes)
	directory, directoryErr := catalog.DirectoryIDFromBytes(directoryBytes)
	generation, generationErr := catalog.DirectoryGenerationFromBytes(generationBytes)
	previous, previousErr := catalog.NewPageCommitment(previousBytes)
	if shareErr != nil || directoryErr != nil || generationErr != nil || previousErr != nil {
		return catalog.CatalogPage{}, ErrWireObject
	}
	var rawEntries []cbor.RawMessage
	if err := catalogWireDec.Unmarshal(fields[7], &rawEntries); err != nil || len(rawEntries) > catalog.MaxCatalogPageEntries {
		return catalog.CatalogPage{}, ErrWireObject
	}
	entries := make([]catalog.Entry, len(rawEntries))
	for index, raw := range rawEntries {
		entry, err := decodeWireEntry(raw)
		if err != nil {
			return catalog.CatalogPage{}, err
		}
		entries[index] = entry
	}
	page, err := catalog.NewCatalogPage(catalog.CatalogPageSpec{
		ShareInstance: share, DirectoryID: directory, Generation: generation,
		PageIndex: uint32(pageIndex), Previous: previous, Entries: entries,
		Terminal: terminal, OmittedCount: omitted,
	}, committer)
	if err != nil {
		return catalog.CatalogPage{}, errors.Join(ErrWireObject, err)
	}
	canonical, _ := encodeCatalogPage(page)
	if !bytes.Equal(canonical, encoded) {
		return catalog.CatalogPage{}, ErrNonCanonicalWire
	}
	return page, nil
}

func encodeWireEntry(entry catalog.Entry) ([]any, error) {
	var expected any
	if entry.Kind() == catalog.NodeKindFile {
		expected = entry.ExpectedSize()
	}
	var seconds any
	var nanos uint64
	var precision uint64
	modified := entry.ModifiedTime()
	if modified.Present() {
		seconds = modified.Seconds()
		nanos = uint64(modified.Nanoseconds())
		precision = uint64(modified.Precision())
	}
	switch entry.Kind() {
	case catalog.NodeKindDirectory:
		if id, ok := entry.DirectoryID(); !ok || id.IsZero() {
			return nil, ErrWireObject
		}
	case catalog.NodeKindFile:
		if id, ok := entry.FileID(); !ok || id.IsZero() {
			return nil, ErrWireObject
		}
	default:
		return nil, ErrWireObject
	}
	return []any{uint64(entry.Kind()), entry.NodeID().Bytes(), entry.Name(), expected, seconds, nanos, precision}, nil
}

func decodeWireEntry(encoded []byte) (catalog.Entry, error) {
	var fields []cbor.RawMessage
	if err := catalogWireDec.Unmarshal(encoded, &fields); err != nil || len(fields) != 7 {
		return catalog.Entry{}, ErrWireObject
	}
	kind, kindErr := wireUint(fields[0])
	idBytes, idErr := wireBytes(fields[1], catalog.IdentityBytes)
	name, nameErr := wireText(fields[2])
	if kindErr != nil || idErr != nil || nameErr != nil {
		return catalog.Entry{}, ErrWireObject
	}
	modified, err := decodeWireModified(fields[4], fields[5], fields[6])
	if err != nil {
		return catalog.Entry{}, err
	}
	switch catalog.NodeKind(kind) {
	case catalog.NodeKindDirectory:
		if !wireNull(fields[3]) {
			return catalog.Entry{}, ErrWireObject
		}
		id, err := catalog.DirectoryIDFromBytes(idBytes)
		if err != nil {
			return catalog.Entry{}, ErrWireObject
		}
		return catalog.NewDirectoryEntry(id, name, modified)
	case catalog.NodeKindFile:
		size, err := wireUint(fields[3])
		if err != nil || size > catalog.MaxFileSize {
			return catalog.Entry{}, ErrWireObject
		}
		id, err := catalog.FileIDFromBytes(idBytes)
		if err != nil {
			return catalog.Entry{}, ErrWireObject
		}
		return catalog.NewFileEntry(id, name, size, modified)
	default:
		return catalog.Entry{}, ErrWireObject
	}
}

func decodeWireModified(seconds, nanos, precision cbor.RawMessage) (catalog.ModifiedTime, error) {
	if wireNull(seconds) {
		nanoseconds, nanosErr := wireUint(nanos)
		valuePrecision, precisionErr := wireUint(precision)
		if nanosErr != nil || precisionErr != nil || nanoseconds != 0 || valuePrecision != 0 {
			return catalog.ModifiedTime{}, ErrWireObject
		}
		return catalog.ModifiedTime{}, nil
	}
	secondsValue, err := wireInt(seconds)
	if err != nil {
		return catalog.ModifiedTime{}, ErrWireObject
	}
	nanoseconds, nanosErr := wireUint(nanos)
	valuePrecision, precisionErr := wireUint(precision)
	if nanosErr != nil || precisionErr != nil || nanoseconds > math.MaxUint32 || valuePrecision > math.MaxUint8 {
		return catalog.ModifiedTime{}, ErrWireObject
	}
	return catalog.NewModifiedTime(secondsValue, uint32(nanoseconds), catalog.TimePrecision(valuePrecision))
}

func decodeWireMap(encoded []byte, exactFields int) (map[uint64]cbor.RawMessage, error) {
	if len(encoded) == 0 {
		return nil, ErrWireObject
	}
	var fields map[uint64]cbor.RawMessage
	if err := catalogWireDec.Unmarshal(encoded, &fields); err != nil || len(fields) != exactFields {
		return nil, ErrWireObject
	}
	for index := range exactFields {
		if fields[uint64(index)] == nil {
			return nil, ErrWireObject
		}
	}
	return fields, nil
}

func wireUint(encoded []byte) (uint64, error) {
	var value uint64
	if err := catalogWireDec.Unmarshal(encoded, &value); err != nil {
		return 0, ErrWireObject
	}
	return value, nil
}

func wireInt(encoded []byte) (int64, error) {
	var value any
	if err := catalogWireDec.Unmarshal(encoded, &value); err != nil {
		return 0, ErrWireObject
	}
	switch typed := value.(type) {
	case int64:
		return typed, nil
	case uint64:
		if typed > math.MaxInt64 {
			return 0, ErrWireObject
		}
		return int64(typed), nil
	default:
		return 0, ErrWireObject
	}
}

func wireBytes(encoded []byte, exact int) ([]byte, error) {
	var value []byte
	if err := catalogWireDec.Unmarshal(encoded, &value); err != nil || exact >= 0 && len(value) != exact {
		return nil, ErrWireObject
	}
	return bytes.Clone(value), nil
}

func wireText(encoded []byte) (string, error) {
	var value string
	if err := catalogWireDec.Unmarshal(encoded, &value); err != nil {
		return "", ErrWireObject
	}
	return value, nil
}

func wireBool(encoded []byte) (bool, error) {
	var value bool
	if err := catalogWireDec.Unmarshal(encoded, &value); err != nil {
		return false, ErrWireObject
	}
	return value, nil
}

func wireNull(encoded []byte) bool { return bytes.Equal(encoded, []byte{0xf6}) }
