package resumestate

import (
	"bytes"
	"crypto/sha256"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"reflect"
	"testing"

	"github.com/windshare/windshare/core/catalog"
	"github.com/windshare/windshare/core/content"
	"github.com/windshare/windshare/core/transfer"
)

func TestCanonicalStateCodecsRoundTripIndependently(t *testing.T) {
	control, err := NewControl(ControlSpec{
		Backend: testBackend(t), OutputRoot: testRootBinding(t),
		Certification: CertificationLinuxExt4ProcessRestart, Durability: transfer.DurabilityProcessRestart,
		Generation: 1,
	})
	if err != nil {
		t.Fatal(err)
	}
	headerAuthority := testSessionAuthority(t, SessionActive)
	header := headerAuthority.Header()
	file := testBoundFileRecord(t, FilePublished)
	tests := []struct {
		name   string
		encode func() ([]byte, error)
		decode func([]byte) (any, error)
		want   any
	}{
		{name: "control", encode: func() ([]byte, error) { return EncodeControl(control) }, decode: func(raw []byte) (any, error) { return DecodeControl(raw) }, want: control},
		{name: "header", encode: func() ([]byte, error) { return EncodeHeader(header) }, decode: func(raw []byte) (any, error) { return DecodeHeader(raw) }, want: header},
		{name: "file", encode: func() ([]byte, error) { return EncodeFileRecord(file) }, decode: func(raw []byte) (any, error) { return DecodeFileRecord(raw) }, want: file.Record()},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			first, err := test.encode()
			if err != nil {
				t.Fatal(err)
			}
			second, err := test.encode()
			if err != nil || !bytes.Equal(first, second) {
				t.Fatalf("encoding is not deterministic: %v", err)
			}
			got, err := test.decode(first)
			if err != nil || !reflect.DeepEqual(got, test.want) {
				t.Fatalf("round trip = %#v, %v; want %#v", got, err, test.want)
			}
		})
	}
}

func TestCanonicalStateCodecGoldenDigests(t *testing.T) {
	controlBytes, err := EncodeControl(testControl(t))
	if err != nil {
		t.Fatal(err)
	}
	headerBytes, err := EncodeHeader(testHeader(t))
	if err != nil {
		t.Fatal(err)
	}
	fileBytes, err := EncodeFileRecord(testBoundFileRecord(t, FilePublished))
	if err != nil {
		t.Fatal(err)
	}
	for _, test := range []struct {
		name string
		raw  []byte
		want string
	}{
		{name: "control", raw: controlBytes, want: "5fd030a017883d1844d1edb37b73acac250b025574b7506664843c709d1813c8"},
		{name: "header", raw: headerBytes, want: "f631a25c51c67c4bc69cab8917f8ab7db652e6c48aa1e5d87578e1f34e9de4d1"},
		{name: "file", raw: fileBytes, want: "b55b3d670a3d8ba013b7506a367bc5be5d80c3f9467cc0fcf77ee1325307db0d"},
	} {
		t.Run(test.name, func(t *testing.T) {
			got := sha256.Sum256(test.raw)
			if encoded := fmt.Sprintf("%x", got); encoded != test.want {
				t.Fatalf("canonical envelope digest = %s", encoded)
			}
		})
	}
}

func TestRecordEnvelopesRejectCorruptionVersionsAndBounds(t *testing.T) {
	headerBytes, err := EncodeHeader(testHeader(t))
	if err != nil {
		t.Fatal(err)
	}
	mutations := []struct {
		name string
		raw  []byte
	}{
		{name: "empty", raw: nil},
		{name: "truncated", raw: append([]byte(nil), headerBytes[:len(headerBytes)-1]...)},
		{name: "oversized", raw: make([]byte, MaxSessionHeaderBytes+1)},
	}
	for _, offset := range []int{0, len(headerMagic), len(headerMagic) + recordLengthBytes, len(headerBytes) - 1} {
		changed := append([]byte(nil), headerBytes...)
		changed[offset] ^= 0x40
		mutations = append(mutations, struct {
			name string
			raw  []byte
		}{name: "flipped", raw: changed})
	}
	lengthMismatch := append([]byte(nil), headerBytes...)
	declared := binary.BigEndian.Uint32(lengthMismatch[len(headerMagic) : len(headerMagic)+recordLengthBytes])
	binary.BigEndian.PutUint32(lengthMismatch[len(headerMagic):len(headerMagic)+recordLengthBytes], declared+1)
	rewriteEnvelopeChecksum(lengthMismatch)
	mutations = append(mutations, struct {
		name string
		raw  []byte
	}{name: "length mismatch", raw: lengthMismatch})
	oldMagic := [8]byte{'W', 'S', 'O', 'H', 'D', 'R', '0', '2'}
	v2, err := encodeEnvelope(oldMagic, storeHeader(testHeader(t)), MaxSessionHeaderBytes)
	if err != nil {
		t.Fatal(err)
	}
	mutations = append(mutations, struct {
		name string
		raw  []byte
	}{name: "v2", raw: v2})
	for _, test := range mutations {
		t.Run(test.name, func(t *testing.T) {
			if _, err := DecodeHeader(test.raw); !errors.Is(err, ErrCorruptState) {
				t.Fatalf("decode error = %v", err)
			}
		})
	}
}

func TestReaderDecodersBoundAllocationBeforeParsing(t *testing.T) {
	control, err := NewControl(ControlSpec{
		Backend: testBackend(t), OutputRoot: testRootBinding(t),
		Certification: CertificationLinuxExt4ProcessRestart, Durability: transfer.DurabilityProcessRestart,
		Generation: 1,
	})
	if err != nil {
		t.Fatal(err)
	}
	controlBytes, _ := EncodeControl(control)
	headerBytes, _ := EncodeHeader(testHeader(t))
	fileBytes, _ := EncodeFileRecord(testBoundFileRecord(t, FilePublished))
	if got, err := ReadControl(bytes.NewReader(controlBytes)); err != nil || got != control {
		t.Fatalf("read control = %+v, %v", got, err)
	}
	if got, err := ReadHeader(bytes.NewReader(headerBytes)); err != nil || got != testHeader(t) {
		t.Fatalf("read header = %+v, %v", got, err)
	}
	if got, err := ReadFileRecord(bytes.NewReader(fileBytes)); err != nil || !reflect.DeepEqual(got, testFileRecord(t, FilePublished)) {
		t.Fatalf("read file = %+v, %v", got, err)
	}
	if _, err := ReadFileRecord(io.LimitReader(repeatingReader{}, int64(MaxFileStateBytes)+1)); !errors.Is(err, ErrCorruptState) {
		t.Fatalf("oversized reader error = %v", err)
	}
	if _, err := ReadHeader(nil); !errors.Is(err, ErrInvalidState) {
		t.Fatalf("nil reader error = %v", err)
	}
	readFailure := errors.New("read failure")
	if _, err := ReadControl(failingReader{err: readFailure}); !errors.Is(err, readFailure) {
		t.Fatalf("reader failure = %v", err)
	}
}

func TestCanonicalDecodersRejectUnknownAndSemanticFieldsWithValidChecksums(t *testing.T) {
	header := storeHeader(testHeader(t))
	unknownHeader := map[uint64]any{
		0: header.Schema, 1: header.Backend, 2: header.SessionID, 3: header.ShareInstance,
		4: header.SyntheticRoot, 5: header.ResumeIntent, 6: header.SelectionIdentity,
		7: header.SelectedDirectoryCount, 8: header.SelectedFileCount, 9: header.OutputRoot,
		10: header.Lifecycle, 11: header.StateGeneration,
		12: header.OutputRootCertification, 13: header.OutputAncestry, 99: "unknown",
	}
	unknown, err := encodeEnvelope(headerMagic, unknownHeader, MaxSessionHeaderBytes)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := DecodeHeader(unknown); !errors.Is(err, ErrCorruptState) {
		t.Fatalf("unknown field error = %v", err)
	}
	header.Schema = 2
	wrongSchema, _ := encodeEnvelope(headerMagic, header, MaxSessionHeaderBytes)
	if _, err := DecodeHeader(wrongSchema); !errors.Is(err, ErrCorruptState) {
		t.Fatalf("schema error = %v", err)
	}
	header = storeHeader(testHeader(t))
	header.ResumeIntent = make([]byte, transfer.ResumeIntentBytes)
	wrongIntent, _ := encodeEnvelope(headerMagic, header, MaxSessionHeaderBytes)
	if _, err := DecodeHeader(wrongIntent); !errors.Is(err, ErrCorruptState) {
		t.Fatalf("resume intent error = %v", err)
	}
	header = storeHeader(testHeader(t))
	header.OutputAncestry = nil
	missingAncestry, _ := encodeEnvelope(headerMagic, header, MaxSessionHeaderBytes)
	if _, err := DecodeHeader(missingAncestry); !errors.Is(err, ErrCorruptState) {
		t.Fatalf("missing ancestry binding error = %v", err)
	}
	header.OutputAncestry = make([]byte, OutputAncestryBindingBytes)
	zeroAncestry, _ := encodeEnvelope(headerMagic, header, MaxSessionHeaderBytes)
	if _, err := DecodeHeader(zeroAncestry); !errors.Is(err, ErrCorruptState) {
		t.Fatalf("zero ancestry binding error = %v", err)
	}

	record := testBoundFileRecord(t, FilePublished)
	raw, err := EncodeFileRecord(record)
	if err != nil {
		t.Fatal(err)
	}
	var stored storedFileRecord
	if err := decodeEnvelope(raw, fileMagic, MaxFileStateBytes, &stored); err != nil {
		t.Fatal(err)
	}
	stored.LocatorDigest[0]++
	wrongDigest, _ := encodeEnvelope(fileMagic, stored, MaxFileStateBytes)
	if _, err := DecodeFileRecord(wrongDigest); !errors.Is(err, ErrCorruptState) {
		t.Fatalf("locator binding error = %v", err)
	}
	stored = storedFileRecord{}
	emptyRecord, _ := encodeEnvelope(fileMagic, stored, MaxFileStateBytes)
	if _, err := DecodeFileRecord(emptyRecord); !errors.Is(err, ErrCorruptState) {
		t.Fatalf("empty record error = %v", err)
	}
}

func TestCanonicalDecoderRejectsDuplicateNonCanonicalAndOverflowFields(t *testing.T) {
	header := storeHeader(testHeader(t))
	payload, err := stateEnc.Marshal(header)
	if err != nil {
		t.Fatal(err)
	}
	if len(payload) == 0 || payload[0] != 0xae {
		t.Fatalf("unexpected header map encoding %x", payload)
	}

	duplicate := append([]byte(nil), payload...)
	duplicate[0] = 0xaf
	key, _ := stateEnc.Marshal(uint64(11))
	value, _ := stateEnc.Marshal(header.StateGeneration)
	duplicate = append(duplicate, key...)
	duplicate = append(duplicate, value...)

	nonCanonical := append([]byte(nil), payload...)
	schemaPrefix := []byte{0xae, 0x00, byte(SchemaVersion)}
	if !bytes.HasPrefix(nonCanonical, schemaPrefix) {
		t.Fatalf("unexpected schema encoding %x", nonCanonical[:min(len(nonCanonical), len(schemaPrefix))])
	}
	nonCanonical = append([]byte{0xae, 0x00, 0x18, byte(SchemaVersion)}, nonCanonical[len(schemaPrefix):]...)

	indefinite := append([]byte(nil), payload...)
	indefinite[0] = 0xbf
	indefinite = append(indefinite, 0xff)

	nonCanonicalOrder := []byte{0xae}
	headerMap := storedHeaderMap(header)
	for mapKey := uint64(13); ; mapKey-- {
		encodedKey, marshalErr := stateEnc.Marshal(mapKey)
		if marshalErr != nil {
			t.Fatal(marshalErr)
		}
		encodedValue, marshalErr := stateEnc.Marshal(headerMap[mapKey])
		if marshalErr != nil {
			t.Fatal(marshalErr)
		}
		nonCanonicalOrder = append(nonCanonicalOrder, encodedKey...)
		nonCanonicalOrder = append(nonCanonicalOrder, encodedValue...)
		if mapKey == 0 {
			break
		}
	}

	tagged := append([]byte{0xc0}, payload...)
	overNested := append(bytes.Repeat([]byte{0x81}, 32), 0xf6)

	overflowMap := storedHeaderMap(header)
	overflowMap[10] = uint64(1 << 8)
	overflowPayload, err := stateEnc.Marshal(overflowMap)
	if err != nil {
		t.Fatal(err)
	}

	for _, test := range []struct {
		name    string
		payload []byte
	}{
		{name: "duplicate field", payload: duplicate},
		{name: "noncanonical integer", payload: nonCanonical},
		{name: "noncanonical map order", payload: nonCanonicalOrder},
		{name: "indefinite map", payload: indefinite},
		{name: "tagged value", payload: tagged},
		{name: "excessive nesting", payload: overNested},
		{name: "integer overflow", payload: overflowPayload},
	} {
		t.Run(test.name, func(t *testing.T) {
			encoded := envelopeForPayload(headerMagic, test.payload)
			if _, err := DecodeHeader(encoded); !errors.Is(err, ErrCorruptState) {
				t.Fatalf("decode error = %v", err)
			}
		})
	}

	valid, err := EncodeHeader(testHeader(t))
	if err != nil {
		t.Fatal(err)
	}
	withTrailingByte := append(append([]byte(nil), valid...), 0)
	if _, err := DecodeHeader(withTrailingByte); !errors.Is(err, ErrCorruptState) {
		t.Fatalf("trailing byte error = %v", err)
	}
}

func TestFileCodecAcceptsExactlyDurableRangeLimitAndRejectsNextRange(t *testing.T) {
	exactSize := uint64(MaxDurableRangesPerFile * 2)
	selection := testSelection(t, exactSize)
	session := testSessionAuthorityForSelection(t, selection, SessionActive)
	descriptor := testDescriptor(t, session, exactSize)
	created, err := NewFileRecord(FileRecordSpec{
		Session: session, Descriptor: descriptor, CanonicalLocator: "folder/file.bin",
		OutputObject: identity32[OutputObjectID](9),
	})
	if err != nil {
		t.Fatal(err)
	}
	witnessed, err := created.Bound().transition(FileTransition{Next: FileWitnessed})
	if err != nil {
		t.Fatal(err)
	}
	resumable, err := BindResumableFile(witnessed, descriptor)
	if err != nil {
		t.Fatal(err)
	}
	ranges := make([]content.Range, MaxDurableRangesPerFile)
	for index := range ranges {
		ranges[index] = content.Range{Offset: uint64(index * 2), End: uint64(index*2 + 1)}
	}
	checkpointed, err := resumable.WithCheckpoint(1, testRanges(t, ranges...))
	if err != nil {
		t.Fatal(err)
	}
	encoded, err := EncodeFileRecord(checkpointed.Bound())
	if err != nil {
		t.Fatal(err)
	}
	decoded, err := DecodeFileRecord(encoded)
	if err != nil || decoded.DurableRanges().Len() != MaxDurableRangesPerFile {
		t.Fatalf("range boundary decode = %d, %v", decoded.DurableRanges().Len(), err)
	}

	var stored storedFileRecord
	if err := decodeEnvelope(encoded, fileMagic, MaxFileStateBytes, &stored); err != nil {
		t.Fatal(err)
	}
	stored.DurableRanges = append(stored.DurableRanges, [2]uint64{exactSize, exactSize + 1})
	stored.ExactSize++
	tooMany, err := encodeEnvelope(fileMagic, stored, MaxFileStateBytes)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := DecodeFileRecord(tooMany); !errors.Is(err, ErrCorruptState) {
		t.Fatalf("range limit + 1 error = %v", err)
	}
}

func TestGlobalControlRejectsUncertifiedDurability(t *testing.T) {
	if _, err := NewControl(ControlSpec{
		Backend: testBackend(t), OutputRoot: testRootBinding(t),
		Certification: "linux/ext4/power-loss/unproven", Durability: transfer.DurabilityPowerLoss,
		Generation: 1,
	}); !errors.Is(err, ErrInvalidState) {
		t.Fatalf("power-loss control error = %v", err)
	}
	stored := storedControl{
		Schema: SchemaVersion, Backend: string(testBackend(t)), OutputRoot: testRootBinding(t).Bytes(),
		Certification: "unknown/filesystem", Durability: uint8(transfer.DurabilityPowerLoss), Generation: 1,
	}
	raw, _ := encodeEnvelope(controlMagic, stored, MaxControlStateBytes)
	if _, err := DecodeControl(raw); !errors.Is(err, ErrCorruptState) {
		t.Fatalf("persisted power-loss error = %v", err)
	}
	stored.Durability = uint8(transfer.DurabilityProcessRestart)
	unknownFilesystem, _ := encodeEnvelope(controlMagic, stored, MaxControlStateBytes)
	if _, err := DecodeControl(unknownFilesystem); !errors.Is(err, ErrCorruptState) {
		t.Fatalf("unknown filesystem error = %v", err)
	}
	for _, value := range []string{"", " leading", "trailing ", string([]byte{0xff}), string(bytes.Repeat([]byte{'x'}, MaxCertificationIDBytes+1))} {
		if _, err := NewCertificationID(value); !errors.Is(err, ErrInvalidState) {
			t.Fatalf("certification %q error = %v", value, err)
		}
	}
}

func TestStateCodecOwnsExternalEnumWireCodes(t *testing.T) {
	for _, test := range []struct {
		durability transfer.DurabilityLevel
		stored     uint8
	}{
		{durability: transfer.DurabilityProcessRestart, stored: 1},
		{durability: transfer.DurabilityPowerLoss, stored: 2},
	} {
		encoded, err := encodeStoredDurability(test.durability)
		decoded, decodeErr := decodeStoredDurability(test.stored)
		if err != nil || decodeErr != nil || encoded != test.stored || decoded != test.durability {
			t.Fatalf("durability %d/%d round trip = %d/%d, %v/%v", test.durability, test.stored, encoded, decoded, err, decodeErr)
		}
	}
	if _, err := encodeStoredDurability(transfer.DurabilityNone); !errors.Is(err, ErrInvalidState) {
		t.Fatalf("unstored durability error = %v", err)
	}
	for _, code := range []uint8{0, 3, 255} {
		if _, err := decodeStoredDurability(code); !errors.Is(err, ErrCorruptState) {
			t.Fatalf("durability code %d error = %v", code, err)
		}
	}

	for _, test := range []struct {
		precision catalog.TimePrecision
		stored    uint8
	}{
		{precision: catalog.TimePrecisionSeconds, stored: 1},
		{precision: catalog.TimePrecisionMilliseconds, stored: 2},
		{precision: catalog.TimePrecisionNanoseconds, stored: 3},
	} {
		decoded, err := decodeStoredTimePrecision(test.stored)
		if err != nil || decoded != test.precision {
			t.Fatalf("precision code %d = %d, %v", test.stored, decoded, err)
		}
	}
	for _, code := range []uint8{0, 4, 255} {
		if _, err := restoreModifiedTime(storedModifiedTime{Present: true, Precision: code}); !errors.Is(err, ErrCorruptState) {
			t.Fatalf("precision code %d error = %v", code, err)
		}
	}
}

func TestChecksumDetectsCorruptionButDoesNotGrantOwnership(t *testing.T) {
	record := testBoundFileRecord(t, FileWitnessed)
	raw, err := EncodeFileRecord(record)
	if err != nil {
		t.Fatal(err)
	}
	decoded, err := DecodeFileRecord(raw)
	if err != nil {
		t.Fatal(err)
	}
	session := testSessionAuthority(t, SessionActive)
	bound := bindTestFileRecord(t, session, decoded)
	decision, err := ReduceFileRecovery(bound, FileObservation{
		Anchor: AnchorMissing, Stage: EntryMissing, Final: EntryMissing,
	})
	if err != nil || decision.NextPhase() != FileQuarantined || decision.QuarantineReason() != QuarantineAnchorMissing {
		t.Fatalf("missing witness decision = %+v, %v", decision, err)
	}
}

func rewriteEnvelopeChecksum(encoded []byte) {
	payloadEnd := len(encoded) - recordChecksumBytes
	checksum := sha256.Sum256(encoded[:payloadEnd])
	copy(encoded[payloadEnd:], checksum[:])
}

func envelopeForPayload(magic [8]byte, payload []byte) []byte {
	encoded := make([]byte, 0, len(magic)+recordLengthBytes+len(payload)+recordChecksumBytes)
	encoded = append(encoded, magic[:]...)
	var length [recordLengthBytes]byte
	binary.BigEndian.PutUint32(length[:], uint32(len(payload)))
	encoded = append(encoded, length[:]...)
	encoded = append(encoded, payload...)
	checksum := sha256.Sum256(encoded)
	return append(encoded, checksum[:]...)
}

func storedHeaderMap(header storedHeader) map[uint64]any {
	return map[uint64]any{
		0: header.Schema, 1: header.Backend, 2: header.SessionID, 3: header.ShareInstance,
		4: header.SyntheticRoot, 5: header.ResumeIntent, 6: header.SelectionIdentity,
		7: header.SelectedDirectoryCount, 8: header.SelectedFileCount, 9: header.OutputRoot,
		10: header.Lifecycle, 11: header.StateGeneration, 12: header.OutputRootCertification,
		13: header.OutputAncestry,
	}
}

type repeatingReader struct{}

func (repeatingReader) Read(destination []byte) (int, error) {
	for index := range destination {
		destination[index] = 1
	}
	return len(destination), nil
}

type failingReader struct{ err error }

func (reader failingReader) Read([]byte) (int, error) { return 0, reader.err }
