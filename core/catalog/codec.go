package catalog

import (
	"bytes"
	"crypto/sha256"
	"errors"
	"fmt"
	"slices"

	"github.com/fxamacker/cbor/v2"
)

const catalogStorageSchema = 3

var ErrCorruptCatalogStorage = errors.New("catalog durable storage is corrupt")

var catalogStorageEnc = func() cbor.EncMode {
	options := cbor.CoreDetEncOptions()
	options.NilContainers = cbor.NilContainerAsEmpty
	mode, err := options.EncMode()
	if err != nil {
		panic(err)
	}
	return mode
}()

var catalogStorageDec = func() cbor.DecMode {
	mode, err := cbor.DecOptions{
		DupMapKey:         cbor.DupMapKeyEnforcedAPF,
		IndefLength:       cbor.IndefLengthForbidden,
		TagsMd:            cbor.TagsForbidden,
		ExtraReturnErrors: cbor.ExtraDecErrorUnknownField,
		FieldNameMatching: cbor.FieldNameMatchingCaseSensitive,
		MaxNestedLevels:   8,
		MaxArrayElements:  MaxCatalogPageEntries,
		MaxMapPairs:       32,
	}.DecMode()
	if err != nil {
		panic(err)
	}
	return mode
}()

type storedModifiedTime struct {
	Present     bool
	Seconds     int64
	Nanoseconds uint32
	Precision   uint8
}

type storedNode struct {
	Schema           uint64
	Kind             uint8
	ID               []byte
	Parent           []byte
	Name             string
	SourceReference  []byte
	SourceIdentity   []byte
	VersionCandidate []byte
	ExpectedSize     uint64
	Modified         storedModifiedTime
	SyntheticRoot    bool
}

func storeModifiedTime(value ModifiedTime) storedModifiedTime {
	return storedModifiedTime{
		Present: value.present, Seconds: value.seconds, Nanoseconds: value.nanoseconds, Precision: uint8(value.precision),
	}
}

func restoreModifiedTime(value storedModifiedTime) (ModifiedTime, error) {
	if !value.Present {
		if value.Seconds != 0 || value.Nanoseconds != 0 || value.Precision != 0 {
			return ModifiedTime{}, fmt.Errorf("%w: absent modified time carries data", ErrCorruptCatalogStorage)
		}
		return ModifiedTime{}, nil
	}
	modified, err := NewModifiedTime(value.Seconds, value.Nanoseconds, TimePrecision(value.Precision))
	if err != nil {
		return ModifiedTime{}, fmt.Errorf("%w: %w", ErrCorruptCatalogStorage, err)
	}
	return modified, nil
}

func encodeNodeRecord(record NodeRecord) ([]byte, error) {
	if !record.valid() {
		return nil, errors.New("cannot persist an invalid catalog node")
	}
	value := storedNode{
		Schema: catalogStorageSchema, Kind: uint8(record.kind), ID: record.nodeID.Bytes(),
		Parent: record.parent.Bytes(), Name: record.name, SourceReference: record.sourceReference.Bytes(),
		SourceIdentity:   record.sourceIdentity.Bytes(),
		VersionCandidate: record.versionCandidate.Bytes(), ExpectedSize: record.expectedSize,
		Modified: storeModifiedTime(record.modified), SyntheticRoot: record.syntheticRoot,
	}
	return catalogStorageEnc.Marshal(value)
}

func decodeNodeRecord(encoded []byte) (NodeRecord, error) {
	var value storedNode
	if err := decodeCanonicalStorage(encoded, &value); err != nil {
		return NodeRecord{}, err
	}
	if value.Schema != catalogStorageSchema {
		return NodeRecord{}, fmt.Errorf("%w: unsupported node schema", ErrCorruptCatalogStorage)
	}
	id, err := NodeIDFromBytes(value.ID)
	if err != nil {
		return NodeRecord{}, fmt.Errorf("%w: invalid node identity", ErrCorruptCatalogStorage)
	}
	modified, err := restoreModifiedTime(value.Modified)
	if err != nil {
		return NodeRecord{}, err
	}
	if value.SyntheticRoot {
		if NodeKind(value.Kind) != NodeKindDirectory || len(value.Parent) != IdentityBytes ||
			!bytes.Equal(value.Parent, make([]byte, IdentityBytes)) ||
			value.Name != "" || len(value.SourceReference) != 0 || len(value.SourceIdentity) != 0 ||
			len(value.VersionCandidate) != 0 || value.ExpectedSize != 0 || value.Modified.Present {
			return NodeRecord{}, fmt.Errorf("%w: malformed synthetic root", ErrCorruptCatalogStorage)
		}
		record, createErr := NewSyntheticRootNodeRecord(DirectoryID(id))
		if createErr != nil {
			return NodeRecord{}, fmt.Errorf("%w: %w", ErrCorruptCatalogStorage, createErr)
		}
		return record, nil
	}
	parent, err := DirectoryIDFromBytes(value.Parent)
	if err != nil {
		return NodeRecord{}, fmt.Errorf("%w: invalid node parent", ErrCorruptCatalogStorage)
	}
	locator, err := NewSourceReference(value.SourceReference)
	if err != nil {
		return NodeRecord{}, fmt.Errorf("%w: %w", ErrCorruptCatalogStorage, err)
	}
	identity, err := NewSourceIdentity(value.SourceIdentity)
	if err != nil {
		return NodeRecord{}, fmt.Errorf("%w: invalid source identity", ErrCorruptCatalogStorage)
	}
	switch NodeKind(value.Kind) {
	case NodeKindDirectory:
		if len(value.VersionCandidate) != 0 || value.ExpectedSize != 0 {
			return NodeRecord{}, fmt.Errorf("%w: directory carries file metadata", ErrCorruptCatalogStorage)
		}
		record, createErr := NewDirectoryNodeRecord(DirectoryID(id), parent, value.Name, locator, identity, modified)
		if createErr != nil {
			return NodeRecord{}, fmt.Errorf("%w: %w", ErrCorruptCatalogStorage, createErr)
		}
		return record, nil
	case NodeKindFile:
		candidate, candidateErr := NewVersionCandidate(value.VersionCandidate)
		if candidateErr != nil {
			return NodeRecord{}, fmt.Errorf("%w: invalid version candidate", ErrCorruptCatalogStorage)
		}
		record, createErr := NewFileNodeRecord(FileID(id), parent, value.Name, locator, identity, candidate, value.ExpectedSize, modified)
		if createErr != nil {
			return NodeRecord{}, fmt.Errorf("%w: %w", ErrCorruptCatalogStorage, createErr)
		}
		return record, nil
	default:
		return NodeRecord{}, fmt.Errorf("%w: unknown node kind", ErrCorruptCatalogStorage)
	}
}

func decodeCanonicalStorage(encoded []byte, target any) error {
	if err := catalogStorageDec.Unmarshal(encoded, target); err != nil {
		return fmt.Errorf("%w: %w", ErrCorruptCatalogStorage, err)
	}
	canonical, err := catalogStorageEnc.Marshal(target)
	if err != nil || !bytes.Equal(canonical, encoded) {
		return fmt.Errorf("%w: non-canonical durable record", ErrCorruptCatalogStorage)
	}
	return nil
}

const PagePayloadSchema = 1

var (
	ErrInvalidPagePayload      = errors.New("invalid catalog page payload")
	ErrNonCanonicalPagePayload = errors.New("non-canonical catalog page payload")
)

type PageAddress struct {
	ShareInstance ShareInstance
	DirectoryID   DirectoryID
	Generation    DirectoryGeneration
	PageIndex     uint32
}

// PagePayload binds owned canonical bytes to the address and commitment
// established by page construction or an addressed storage read.
type PagePayload struct {
	address    PageAddress
	commitment PageCommitment
	encoded    []byte
}

func (p PagePayload) Address() PageAddress       { return p.address }
func (p PagePayload) Commitment() PageCommitment { return p.commitment }
func (p PagePayload) Bytes() []byte              { return slices.Clone(p.encoded) }
func (p PagePayload) IsZero() bool               { return len(p.encoded) == 0 }

var pageWireEnc = func() cbor.EncMode {
	mode, err := cbor.CoreDetEncOptions().EncMode()
	if err != nil {
		panic(err)
	}
	return mode
}()

var pageWireDec = func() cbor.DecMode {
	mode, err := cbor.DecOptions{
		DupMapKey:         cbor.DupMapKeyEnforcedAPF,
		IndefLength:       cbor.IndefLengthForbidden,
		TagsMd:            cbor.TagsForbidden,
		ExtraReturnErrors: cbor.ExtraDecErrorUnknownField,
		MaxNestedLevels:   8,
		MaxArrayElements:  MaxCatalogPageEntries,
		MaxMapPairs:       16,
	}.DecMode()
	if err != nil {
		panic(err)
	}
	return mode
}()

type wireCatalogPage struct {
	Schema     uint64             `cbor:"0,keyasint"`
	Share      []byte             `cbor:"1,keyasint"`
	Directory  []byte             `cbor:"2,keyasint"`
	Generation []byte             `cbor:"3,keyasint"`
	Index      uint32             `cbor:"4,keyasint"`
	Terminal   bool               `cbor:"5,keyasint"`
	Previous   []byte             `cbor:"6,keyasint"`
	Entries    []wireCatalogEntry `cbor:"7,keyasint"`
	Omitted    uint64             `cbor:"8,keyasint"`
}

type wireCatalogEntry struct {
	_         struct{} `cbor:",toarray"`
	Kind      NodeKind
	Node      []byte
	Name      string
	Size      *uint64
	Seconds   *int64
	Nanos     uint32
	Precision TimePrecision
}

func encodePagePayload(page CatalogPage) (PagePayload, error) {
	encoded, err := marshalCatalogPage(page)
	if err != nil {
		return PagePayload{}, err
	}
	if len(encoded) > MaxCatalogPayloadBytes {
		return PagePayload{}, ErrPageLimit
	}
	return PagePayload{
		address: page.Address(), commitment: PageCommitment(sha256.Sum256(encoded)), encoded: encoded,
	}, nil
}

func marshalCatalogPage(page CatalogPage) ([]byte, error) {
	entries := make([]wireCatalogEntry, len(page.entries))
	for index := range page.entries {
		entry := &page.entries[index]
		wire := wireCatalogEntry{Kind: entry.kind, Node: entry.nodeID[:], Name: entry.name}
		if entry.kind == NodeKindFile {
			wire.Size = &entry.expectedSize
		}
		if entry.modified.Present() {
			wire.Seconds = &entry.modified.seconds
			wire.Nanos = entry.modified.nanoseconds
			wire.Precision = entry.modified.precision
		}
		entries[index] = wire
	}
	return pageWireEnc.Marshal(wireCatalogPage{
		Schema: PagePayloadSchema, Share: page.shareInstance[:], Directory: page.directoryID[:], Generation: page.generation[:],
		Index: page.pageIndex, Terminal: page.terminal, Previous: page.previous[:], Entries: entries, Omitted: page.omittedCount,
	})
}

// DecodeCatalogPage admits a canonical page from authenticated wire bytes.
func DecodeCatalogPage(encoded []byte) (CatalogPage, error) {
	if len(encoded) == 0 || len(encoded) > MaxCatalogPayloadBytes {
		return CatalogPage{}, ErrInvalidPagePayload
	}
	return decodeCatalogPage(encoded, PageCommitment(sha256.Sum256(encoded)))
}

func (p PagePayload) decode() (CatalogPage, error) {
	page, err := decodeCatalogPage(p.encoded, p.commitment)
	if err != nil || page.Address() != p.address {
		return CatalogPage{}, errors.Join(ErrCorruptCatalogStorage, err)
	}
	return page, nil
}

func decodeCatalogPage(encoded []byte, commitment PageCommitment) (CatalogPage, error) {
	var wire wireCatalogPage
	if err := pageWireDec.Unmarshal(encoded, &wire); err != nil {
		return CatalogPage{}, errors.Join(ErrInvalidPagePayload, err)
	}
	if wire.Schema != PagePayloadSchema || wire.Entries == nil {
		return CatalogPage{}, ErrInvalidPagePayload
	}
	share, shareErr := ShareInstanceFromBytes(wire.Share)
	directory, directoryErr := DirectoryIDFromBytes(wire.Directory)
	generation, generationErr := DirectoryGenerationFromBytes(wire.Generation)
	previous, previousErr := NewPageCommitment(wire.Previous)
	if err := errors.Join(shareErr, directoryErr, generationErr, previousErr); err != nil {
		return CatalogPage{}, errors.Join(ErrInvalidPagePayload, err)
	}
	entries := make([]Entry, len(wire.Entries))
	for index, item := range wire.Entries {
		entry, err := item.entry()
		if err != nil {
			return CatalogPage{}, errors.Join(ErrInvalidPagePayload, err)
		}
		entries[index] = entry
	}
	page, err := prepareCatalogPage(CatalogPageSpec{
		ShareInstance: share, DirectoryID: directory, Generation: generation, PageIndex: wire.Index,
		Previous: previous, Entries: entries, Terminal: wire.Terminal, OmittedCount: wire.Omitted,
	})
	if err != nil {
		return CatalogPage{}, errors.Join(ErrInvalidPagePayload, err)
	}
	canonical, err := marshalCatalogPage(page)
	if err != nil {
		return CatalogPage{}, errors.Join(ErrInvalidPagePayload, err)
	}
	if !bytes.Equal(canonical, encoded) {
		return CatalogPage{}, ErrNonCanonicalPagePayload
	}
	page.commitment = commitment
	return page, nil
}

func (wire wireCatalogEntry) entry() (Entry, error) {
	modified, err := wire.modifiedTime()
	if err != nil {
		return Entry{}, err
	}
	switch wire.Kind {
	case NodeKindDirectory:
		if wire.Size != nil {
			return Entry{}, ErrInvalidPagePayload
		}
		id, err := DirectoryIDFromBytes(wire.Node)
		if err != nil {
			return Entry{}, err
		}
		return NewDirectoryEntry(id, wire.Name, modified)
	case NodeKindFile:
		if wire.Size == nil {
			return Entry{}, ErrInvalidPagePayload
		}
		id, err := FileIDFromBytes(wire.Node)
		if err != nil {
			return Entry{}, err
		}
		return NewFileEntry(id, wire.Name, *wire.Size, modified)
	default:
		return Entry{}, ErrInvalidPagePayload
	}
}

func (wire wireCatalogEntry) modifiedTime() (ModifiedTime, error) {
	if wire.Seconds != nil {
		return NewModifiedTime(*wire.Seconds, wire.Nanos, wire.Precision)
	}
	if wire.Nanos != 0 || wire.Precision != 0 {
		return ModifiedTime{}, ErrInvalidPagePayload
	}
	return ModifiedTime{}, nil
}
