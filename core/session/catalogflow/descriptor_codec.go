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
		!catalog.ValidChunkSize(descriptor.ChunkSize()) ||
		len(descriptor.SenderPublicKey()) != catalog.SenderPublicKeySize ||
		descriptor.CreatedAtSeconds() > catalog.MaxSafeInteger || descriptor.PathPolicy() != catalog.PathPolicy {
		return ErrWireObject
	}
	return nil
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
