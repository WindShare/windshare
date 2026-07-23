package catalog

import (
	"bytes"
	"errors"
	"fmt"

	"github.com/fxamacker/cbor/v2"
)

const catalogStorageSchema = 2

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

type storedEntry struct {
	Kind         uint8
	ID           []byte
	Name         string
	ExpectedSize uint64
	Modified     storedModifiedTime
}

type storedNode struct {
	Schema           uint64
	Kind             uint8
	ID               []byte
	Parent           []byte
	Name             string
	RootSlot         uint16
	RelativePath     string
	SourceIdentity   []byte
	VersionCandidate []byte
	ExpectedSize     uint64
	Modified         storedModifiedTime
	SyntheticRoot    bool
}

type storedPage struct {
	Schema        uint64
	ShareInstance []byte
	DirectoryID   []byte
	Generation    []byte
	PageIndex     uint32
	Previous      []byte
	Entries       []storedEntry
	Terminal      bool
	OmittedCount  uint64
	Commitment    []byte
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

func storeEntry(entry Entry) storedEntry {
	return storedEntry{
		Kind: uint8(entry.kind), ID: entry.nodeID.Bytes(), Name: entry.name,
		ExpectedSize: entry.expectedSize, Modified: storeModifiedTime(entry.modified),
	}
}

func restoreEntry(value storedEntry) (Entry, error) {
	modified, err := restoreModifiedTime(value.Modified)
	if err != nil {
		return Entry{}, err
	}
	switch NodeKind(value.Kind) {
	case NodeKindDirectory:
		id, parseErr := DirectoryIDFromBytes(value.ID)
		if parseErr != nil || value.ExpectedSize != 0 {
			return Entry{}, fmt.Errorf("%w: invalid stored directory entry", ErrCorruptCatalogStorage)
		}
		entry, createErr := NewDirectoryEntry(id, value.Name, modified)
		if createErr != nil {
			return Entry{}, fmt.Errorf("%w: %w", ErrCorruptCatalogStorage, createErr)
		}
		return entry, nil
	case NodeKindFile:
		id, parseErr := FileIDFromBytes(value.ID)
		if parseErr != nil {
			return Entry{}, fmt.Errorf("%w: invalid stored file identity", ErrCorruptCatalogStorage)
		}
		entry, createErr := NewFileEntry(id, value.Name, value.ExpectedSize, modified)
		if createErr != nil {
			return Entry{}, fmt.Errorf("%w: %w", ErrCorruptCatalogStorage, createErr)
		}
		return entry, nil
	default:
		return Entry{}, fmt.Errorf("%w: unknown stored entry kind", ErrCorruptCatalogStorage)
	}
}

func encodeNodeRecord(record NodeRecord) ([]byte, error) {
	if !record.valid() {
		return nil, errors.New("cannot persist an invalid catalog node")
	}
	value := storedNode{
		Schema: catalogStorageSchema, Kind: uint8(record.kind), ID: record.nodeID.Bytes(),
		Parent: record.parent.Bytes(), Name: record.name, RootSlot: uint16(record.locator.rootSlot),
		RelativePath: record.locator.relativePath, SourceIdentity: record.sourceIdentity.Bytes(),
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
			!bytes.Equal(value.Parent, make([]byte, IdentityBytes)) || value.RootSlot != 0 ||
			value.Name != "" || value.RelativePath != "" || len(value.SourceIdentity) != 0 ||
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
	locator, err := NewLocator(RootSlot(value.RootSlot), value.RelativePath)
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

func encodeCatalogPage(page CatalogPage) ([]byte, error) {
	if page.commitment.IsZero() {
		return nil, errors.New("cannot persist an invalid catalog page")
	}
	entries := make([]storedEntry, len(page.entries))
	for index, entry := range page.entries {
		entries[index] = storeEntry(entry)
	}
	value := storedPage{
		Schema: catalogStorageSchema, ShareInstance: page.shareInstance.Bytes(), DirectoryID: page.directoryID.Bytes(),
		Generation: page.generation.Bytes(), PageIndex: page.pageIndex, Previous: page.previous.Bytes(),
		Entries: entries, Terminal: page.terminal, OmittedCount: page.omittedCount, Commitment: page.commitment.Bytes(),
	}
	return catalogStorageEnc.Marshal(value)
}

func decodeCatalogPage(encoded []byte) (CatalogPage, error) {
	var value storedPage
	if err := decodeCanonicalStorage(encoded, &value); err != nil {
		return CatalogPage{}, err
	}
	if value.Schema != catalogStorageSchema {
		return CatalogPage{}, fmt.Errorf("%w: unsupported page schema", ErrCorruptCatalogStorage)
	}
	share, shareErr := ShareInstanceFromBytes(value.ShareInstance)
	directory, directoryErr := DirectoryIDFromBytes(value.DirectoryID)
	generation, generationErr := DirectoryGenerationFromBytes(value.Generation)
	previous, previousErr := NewPageCommitment(value.Previous)
	commitment, commitmentErr := NewPageCommitment(value.Commitment)
	if shareErr != nil || directoryErr != nil || generationErr != nil || previousErr != nil || commitmentErr != nil ||
		share.IsZero() || directory.IsZero() || generation.IsZero() || commitment.IsZero() {
		return CatalogPage{}, fmt.Errorf("%w: invalid page identity", ErrCorruptCatalogStorage)
	}
	entries := make([]Entry, len(value.Entries))
	for index, stored := range value.Entries {
		entry, err := restoreEntry(stored)
		if err != nil {
			return CatalogPage{}, err
		}
		entries[index] = entry
	}
	if err := validateEntryOrder(entries); err != nil {
		return CatalogPage{}, fmt.Errorf("%w: %w", ErrCorruptCatalogStorage, err)
	}
	if len(entries) == 0 && (!value.Terminal || value.PageIndex != 0) ||
		value.PageIndex == 0 && !previous.IsZero() || value.PageIndex > 0 && previous.IsZero() ||
		!value.Terminal && value.OmittedCount != 0 {
		return CatalogPage{}, fmt.Errorf("%w: invalid stored page sequence", ErrCorruptCatalogStorage)
	}
	return CatalogPage{
		shareInstance: share, directoryID: directory, generation: generation, pageIndex: value.PageIndex,
		previous: previous, entries: entries, terminal: value.Terminal, omittedCount: value.OmittedCount,
		commitment: commitment,
	}, nil
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
