package catalog

import (
	"bytes"
	"crypto/sha256"
	"errors"
	"slices"

	"github.com/fxamacker/cbor/v2"
)

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
// established by page construction.
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
	if err != nil {
		return CatalogPage{}, err
	}
	if page.Address() != p.address {
		return CatalogPage{}, ErrInvalidPagePayload
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
