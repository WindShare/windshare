package records

import (
	"bytes"
	"errors"
	"github.com/fxamacker/cbor/v2"
	"maps"
	"testing"

	"github.com/windshare/windshare/core/catalog"
	"github.com/windshare/windshare/core/content"
	"github.com/windshare/windshare/core/senderauth"
)

type recordFixture struct {
	share      catalog.ShareInstance
	file       catalog.FileID
	revision   content.FileRevision
	descriptor content.FileRevisionDescriptor
}

func fixedRecordID[T ~[16]byte](value byte) T {
	var id T
	id[0] = value
	return id
}

func newRecordFixture(t *testing.T, size uint64) recordFixture {
	t.Helper()
	share := fixedRecordID[catalog.ShareInstance](1)
	file := fixedRecordID[catalog.FileID](2)
	revision := fixedRecordID[content.FileRevision](3)
	geometry, err := content.NewFileGeometry(size, catalog.MinChunkSize)
	if err != nil {
		t.Fatal(err)
	}
	descriptor, err := content.NewFileRevisionDescriptor(share, file, revision, geometry, catalog.ModifiedTime{})
	if err != nil {
		t.Fatal(err)
	}
	return recordFixture{share: share, file: file, revision: revision, descriptor: descriptor}
}

func TestRecordCodecRejectsNonCanonicalAndMalformedObjects(t *testing.T) {
	fixture := newRecordFixture(t, 7)
	decoder := Decoder{ShareInstance: fixture.share}
	// The map is valid CBOR but uses a non-minimal integer for schema version.
	nonCanonical := []byte{0xa9, 0x18, 0x00, 0x02, 0x01, 0x50}
	nonCanonical = append(nonCanonical, fixture.share.Bytes()...)
	nonCanonical = append(nonCanonical, 0x02, 0x50)
	nonCanonical = append(nonCanonical, fixture.file.Bytes()...)
	nonCanonical = append(nonCanonical, 0x03, 0x50)
	nonCanonical = append(nonCanonical, fixture.revision.Bytes()...)
	nonCanonical = append(nonCanonical, 0x04, 0x07, 0x05, 0xf6, 0x06, 0x00, 0x07, 0x00)
	nonCanonical = append(nonCanonical, 0x08, 0x58, 0x20)
	nonCanonical = append(nonCanonical, make([]byte, 32)...)
	if _, err := decoder.DecodeRevision(fixture.file, catalog.MinChunkSize, nonCanonical); !errors.Is(err, ErrNonCanonicalObject) {
		t.Fatalf("noncanonical object error = %v", err)
	}

	valid, _ := (Encoder{}).EncodeRevision(fixture.descriptor)
	malformed := [][]byte{
		nil,
		valid[:len(valid)-1],
		append(bytes.Clone(valid), 0),
		func() []byte { value := bytes.Clone(valid); value[1] = 1; return value }(),
		func() []byte { value := bytes.Clone(valid); clear(value[4:8]); return value }(),
	}
	for index, candidate := range malformed {
		if _, err := decoder.DecodeRevision(fixture.file, catalog.MinChunkSize, candidate); err == nil {
			t.Fatalf("malformed object %d accepted", index)
		}
	}

	decoded := map[uint64]any{0: uint64(1)}
	short, _ := cbor.Marshal(decoded)
	if _, err := decoder.DecodeRevision(fixture.file, catalog.MinChunkSize, short); !errors.Is(err, ErrInvalidRecord) {
		t.Fatalf("missing fields error = %v", err)
	}
}

func TestRecordConstructorsRejectInvalidInputsAndOwnBytes(t *testing.T) {
	fixture := newRecordFixture(t, 3)
	data := []byte{1, 2, 3}
	record, err := NewBlockRecord(fixture.descriptor, 0, data)
	if err != nil {
		t.Fatal(err)
	}
	data[0] = 9
	owned := record.Data()
	owned[1] = 9
	if record.DataLength() != 3 || !bytes.Equal(record.Data(), []byte{1, 2, 3}) {
		t.Fatal("block record did not own input/output bytes")
	}
	if _, err := NewBlockRecord(content.FileRevisionDescriptor{}, 0, nil); !errors.Is(err, ErrInvalidRecord) {
		t.Fatalf("zero descriptor error = %v", err)
	}
	if _, err := NewBlockRecord(fixture.descriptor, 0, []byte{1}); !errors.Is(err, ErrInvalidRecord) {
		t.Fatalf("wrong data length error = %v", err)
	}
	if _, err := RecordIDFromBytes(make([]byte, 15)); err == nil {
		t.Fatal("short record identity accepted")
	}
	id := RecordIDFromObject([]byte("record"))
	parsed, err := RecordIDFromBytes(id.Bytes())
	if err != nil || parsed != id || parsed.IsZero() {
		t.Fatalf("record identity roundtrip = %x, %v", parsed, err)
	}
}

func TestRevisionCodecPreservesOptionalModifiedTimeAndRejectsHostileFields(t *testing.T) {
	fixture := newRecordFixture(t, 9)
	modified, err := catalog.NewModifiedTime(-123, 456_000_000, catalog.TimePrecisionNanoseconds)
	if err != nil {
		t.Fatal(err)
	}
	descriptor, _ := content.NewFileRevisionDescriptor(
		fixture.share, fixture.file, fixture.revision, fixture.descriptor.Geometry(), modified,
	)
	plaintext, err := encodeRevision(descriptor)
	if err != nil {
		t.Fatal(err)
	}
	decoded, err := decodeRevision(plaintext, fixture.share, fixture.file, catalog.MinChunkSize)
	if err != nil || decoded != descriptor {
		t.Fatalf("modified revision=%+v err=%v", decoded, err)
	}

	base := map[uint64]any{
		0: uint64(SchemaVersion), 1: fixture.share.Bytes(), 2: fixture.file.Bytes(), 3: fixture.revision.Bytes(),
		4: uint64(9), 5: int64(-123), 6: uint64(456_000_000), 7: uint64(catalog.TimePrecisionNanoseconds),
	}
	tests := []func(map[uint64]any){
		func(fields map[uint64]any) { fields[0] = uint64(2) },
		func(fields map[uint64]any) { fields[1] = make([]byte, 15) },
		func(fields map[uint64]any) { fields[2] = make([]byte, 15) },
		func(fields map[uint64]any) { fields[3] = make([]byte, 15) },
		func(fields map[uint64]any) { fields[4] = uint64(catalog.MaxFileSize) + 1 },
		func(fields map[uint64]any) { fields[5] = nil; fields[6] = uint64(1) },
		func(fields map[uint64]any) { fields[6] = uint64(1) << 32 },
		func(fields map[uint64]any) { fields[7] = uint64(255) },
		func(fields map[uint64]any) { delete(fields, 7) },
	}
	for index, mutate := range tests {
		fields := make(map[uint64]any, len(base))
		maps.Copy(fields, base)
		mutate(fields)
		encoded, _ := recordEncMode.Marshal(fields)
		if _, err := decodeRevision(encoded, fixture.share, fixture.file, catalog.MinChunkSize); err == nil {
			t.Fatalf("hostile revision case %d accepted", index)
		}
	}
	if _, err := decodeRevision(plaintext, fixture.share, fixture.file, catalog.MinChunkSize+1); err == nil {
		t.Fatal("invalid chunk size accepted")
	}
}

func TestBlockCodecRejectsEverySemanticSubstitution(t *testing.T) {
	fixture := newRecordFixture(t, 4)
	base := map[uint64]any{
		0: uint64(SchemaVersion), 1: fixture.share.Bytes(), 2: fixture.file.Bytes(), 3: fixture.revision.Bytes(),
		4: uint64(0), 5: []byte{1, 2, 3, 4},
	}
	valid, _ := recordEncMode.Marshal(base)
	record, err := decodeBlock(valid, fixture.descriptor, 0)
	if err != nil || record.Descriptor() != fixture.descriptor || record.LocalBlockIndex() != 0 {
		t.Fatalf("valid block=%+v err=%v", record, err)
	}
	tests := []func(map[uint64]any){
		func(fields map[uint64]any) { fields[0] = uint64(0) },
		func(fields map[uint64]any) { fields[1] = flowBytes(9) },
		func(fields map[uint64]any) { fields[2] = flowBytes(8) },
		func(fields map[uint64]any) { fields[3] = flowBytes(7) },
		func(fields map[uint64]any) { fields[4] = uint64(1) },
		func(fields map[uint64]any) { fields[5] = []byte{1} },
		func(fields map[uint64]any) { fields[5] = "not bytes" },
		func(fields map[uint64]any) { fields[6] = uint64(0) },
	}
	for index, mutate := range tests {
		fields := make(map[uint64]any, len(base)+1)
		maps.Copy(fields, base)
		mutate(fields)
		encoded, _ := recordEncMode.Marshal(fields)
		if _, err := decodeBlock(encoded, fixture.descriptor, 0); err == nil {
			t.Fatalf("hostile block case %d accepted", index)
		}
	}
}

func flowBytes(value byte) []byte {
	result := make([]byte, 16)
	result[0] = value
	return result
}

func checkedSender(t testing.TB, publicKey []byte) *senderauth.Verifier {
	t.Helper()
	sender, err := senderauth.NewVerifier(publicKey)
	if err != nil {
		t.Fatal(err)
	}
	return sender
}
