package records

import (
	"bytes"
	"crypto/ed25519"
	"errors"
	"io"
	"sync"
	"testing"

	"github.com/fxamacker/cbor/v2"
	"github.com/windshare/windshare/core/catalog"
	"github.com/windshare/windshare/core/content"
	"github.com/windshare/windshare/core/senderobject"
)

type recordFixture struct {
	share      catalog.ShareInstance
	file       catalog.FileID
	revision   content.FileRevision
	descriptor content.FileRevisionDescriptor
	keys       *content.KeyTree
	privateKey ed25519.PrivateKey
	publicKey  ed25519.PublicKey
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
	keys, err := content.NewKeyTree(bytes.Repeat([]byte{0x44}, content.ReadSecretBytes), share)
	if err != nil {
		t.Fatal(err)
	}
	privateKey := ed25519.NewKeyFromSeed(bytes.Repeat([]byte{0x55}, ed25519.SeedSize))
	return recordFixture{
		share: share, file: file, revision: revision, descriptor: descriptor, keys: keys,
		privateKey: privateKey, publicKey: privateKey.Public().(ed25519.PublicKey),
	}
}

func (fixture recordFixture) sealer(t *testing.T, nonce io.Reader, limit uint64) *Sealer {
	t.Helper()
	sealer, err := NewSealer(SealerConfig{
		ShareInstance: fixture.share, Keys: fixture.keys, SigningKey: fixture.privateKey,
		NonceSource: nonce, MaxSealsPerKey: limit,
	})
	if err != nil {
		t.Fatal(err)
	}
	return sealer
}

func (fixture recordFixture) opener(t *testing.T) *Opener {
	t.Helper()
	opener, err := NewOpener(OpenerConfig{ShareInstance: fixture.share, Keys: fixture.keys, VerificationKey: fixture.publicKey})
	if err != nil {
		t.Fatal(err)
	}
	return opener
}

func TestRevisionObjectIsSealOnceAndHidesExactSize(t *testing.T) {
	fixture := newRecordFixture(t, 12_345)
	sealer := fixture.sealer(t, bytes.NewReader(bytes.Repeat([]byte{0x11}, ObjectNonceBytes*2)), 0)
	first, err := sealer.SealRevision(fixture.descriptor)
	if err != nil {
		t.Fatal(err)
	}
	second, err := sealer.SealRevision(fixture.descriptor)
	if err != nil || !bytes.Equal(first, second) {
		t.Fatalf("revision replay changed: err=%v equal=%v", err, bytes.Equal(first, second))
	}
	if bytes.Contains(first, fixture.file.Bytes()) || bytes.Contains(first, fixture.revision.Bytes()) {
		t.Fatal("sealed revision leaked a semantic identity")
	}
	opened, err := fixture.opener(t).OpenRevision(fixture.file, catalog.MinChunkSize, first)
	if err != nil || opened != fixture.descriptor || opened.BlockCountFieldPresent() {
		t.Fatalf("opened revision = %+v, %v", opened, err)
	}

	changedGeometry, _ := content.NewFileGeometry(12_346, catalog.MinChunkSize)
	changed, _ := content.NewFileRevisionDescriptor(fixture.share, fixture.file, fixture.revision, changedGeometry, catalog.ModifiedTime{})
	if _, err := sealer.SealRevision(changed); !errors.Is(err, ErrObjectIdentity) {
		t.Fatalf("changed seal-once descriptor error = %v", err)
	}
	otherFile := fixedRecordID[catalog.FileID](9)
	if _, err := fixture.opener(t).OpenRevision(otherFile, catalog.MinChunkSize, first); !errors.Is(err, ErrObjectSignature) {
		t.Fatalf("wrong file error = %v", err)
	}
}

func TestBlockObjectAuthenticatesEveryFileLocalIdentityAxis(t *testing.T) {
	fixture := newRecordFixture(t, uint64(catalog.MinChunkSize)+3)
	data := bytes.Repeat([]byte{0x72}, catalog.MinChunkSize)
	record, err := NewBlockRecord(fixture.descriptor, 0, data)
	if err != nil {
		t.Fatal(err)
	}
	sealer := fixture.sealer(t, bytes.NewReader(bytes.Repeat([]byte{0x22}, ObjectNonceBytes*8)), 8)
	sealed, err := sealer.SealBlock(record)
	if err != nil {
		t.Fatal(err)
	}
	if sealed.ID != RecordIDFromObject(sealed.Object) || len(sealed.Object) > MaxBlockRecordObjectBytes {
		t.Fatal("sealed record identity or size is wrong")
	}
	opened, err := fixture.opener(t).OpenBlock(fixture.descriptor, 0, sealed.Object)
	if err != nil || !bytes.Equal(opened.Data(), data) {
		t.Fatalf("open block: %v", err)
	}

	changedFile := fixedRecordID[catalog.FileID](8)
	changedFileDescriptor, _ := content.NewFileRevisionDescriptor(fixture.share, changedFile, fixture.revision, fixture.descriptor.Geometry(), catalog.ModifiedTime{})
	changedRevision := fixedRecordID[content.FileRevision](7)
	changedRevisionDescriptor, _ := content.NewFileRevisionDescriptor(fixture.share, fixture.file, changedRevision, fixture.descriptor.Geometry(), catalog.ModifiedTime{})
	for name, descriptor := range map[string]content.FileRevisionDescriptor{
		"file": changedFileDescriptor, "revision": changedRevisionDescriptor,
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := fixture.opener(t).OpenBlock(descriptor, 0, sealed.Object); !errors.Is(err, ErrObjectSignature) {
				t.Fatalf("identity substitution error = %v", err)
			}
		})
	}
	if _, err := fixture.opener(t).OpenBlock(fixture.descriptor, 1, sealed.Object); !errors.Is(err, ErrObjectSignature) {
		t.Fatalf("index substitution error = %v", err)
	}
	tampered := bytes.Clone(sealed.Object)
	tampered[ObjectHeaderBytes+ObjectNonceBytes] ^= 1
	if _, err := fixture.opener(t).OpenBlock(fixture.descriptor, 0, tampered); !errors.Is(err, ErrObjectSignature) {
		t.Fatalf("ciphertext tamper error = %v", err)
	}
}

func TestRecordCodecRejectsNonCanonicalAndMalformedObjects(t *testing.T) {
	fixture := newRecordFixture(t, 7)
	sealer := fixture.sealer(t, bytes.NewReader(bytes.Repeat([]byte{0x33}, ObjectNonceBytes*8)), 8)
	fileKey, err := fixture.keys.FileObjectKey(fixture.file)
	if err != nil {
		t.Fatal(err)
	}
	defer fileKey.Destroy()
	binding, err := senderobject.NewRevisionBinding(fixture.share.Bytes(), fixture.file.Bytes())
	if err != nil {
		t.Fatal(err)
	}
	// The map is valid CBOR but uses a non-minimal integer for schema version.
	nonCanonical := []byte{0xa8, 0x18, 0x00, 0x01, 0x01, 0x50}
	nonCanonical = append(nonCanonical, fixture.share.Bytes()...)
	nonCanonical = append(nonCanonical, 0x02, 0x50)
	nonCanonical = append(nonCanonical, fixture.file.Bytes()...)
	nonCanonical = append(nonCanonical, 0x03, 0x50)
	nonCanonical = append(nonCanonical, fixture.revision.Bytes()...)
	nonCanonical = append(nonCanonical, 0x04, 0x07, 0x05, 0xf6, 0x06, 0x00, 0x07, 0x00)
	object, err := sealer.sealObject(binding, fileKey.Bytes(), nonCanonical)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := fixture.opener(t).OpenRevision(fixture.file, catalog.MinChunkSize, object); !errors.Is(err, ErrNonCanonicalObject) {
		t.Fatalf("noncanonical object error = %v", err)
	}

	valid, _ := sealer.SealRevision(fixture.descriptor)
	malformed := [][]byte{
		nil,
		valid[:ObjectFixedOverheadBytes],
		append(bytes.Clone(valid), 0),
		func() []byte { value := bytes.Clone(valid); value[1] = 1; return value }(),
		func() []byte { value := bytes.Clone(valid); clear(value[4:8]); return value }(),
	}
	for index, candidate := range malformed {
		if _, err := fixture.opener(t).OpenRevision(fixture.file, catalog.MinChunkSize, candidate); err == nil {
			t.Fatalf("malformed object %d accepted", index)
		}
	}

	decoded := map[uint64]any{0: uint64(1)}
	short, _ := cbor.Marshal(decoded)
	shortObject, _ := sealer.sealObject(binding, fileKey.Bytes(), short)
	if _, err := fixture.opener(t).OpenRevision(fixture.file, catalog.MinChunkSize, shortObject); !errors.Is(err, ErrInvalidRecord) {
		t.Fatalf("missing fields error = %v", err)
	}
}

func TestSealAccountingRollsBackFailureAndIsConcurrentSafe(t *testing.T) {
	fixture := newRecordFixture(t, catalog.MinChunkSize)
	record, _ := NewBlockRecord(fixture.descriptor, 0, make([]byte, catalog.MinChunkSize))
	broken := fixture.sealer(t, bytes.NewReader(nil), 1)
	if _, err := broken.SealBlock(record); !errors.Is(err, ErrNonceSource) {
		t.Fatalf("nonce error = %v", err)
	}
	broken.nonceSource = bytes.NewReader(bytes.Repeat([]byte{1}, ObjectNonceBytes))
	if _, err := broken.SealBlock(record); err != nil {
		t.Fatalf("failed attempt consumed seal budget: %v", err)
	}
	if _, err := broken.SealBlock(record); !errors.Is(err, ErrSealLimit) {
		t.Fatalf("seal limit error = %v", err)
	}

	concurrent := fixture.sealer(t, bytes.NewReader(bytes.Repeat([]byte{2}, ObjectNonceBytes*2)), 1)
	var successes, limits int
	var mu sync.Mutex
	var wait sync.WaitGroup
	for range 2 {
		wait.Add(1)
		go func() {
			defer wait.Done()
			_, err := concurrent.SealBlock(record)
			mu.Lock()
			defer mu.Unlock()
			if err == nil {
				successes++
			} else if errors.Is(err, ErrSealLimit) {
				limits++
			}
		}()
	}
	wait.Wait()
	if successes != 1 || limits != 1 {
		t.Fatalf("concurrent results successes=%d limits=%d", successes, limits)
	}
}

func TestRevisionSealOnceReplayDoesNotBypassPerKeyAccounting(t *testing.T) {
	fixture := newRecordFixture(t, 7)
	sealer := fixture.sealer(t, bytes.NewReader(bytes.Repeat([]byte{3}, ObjectNonceBytes)), 1)
	first, err := sealer.SealRevision(fixture.descriptor)
	if err != nil {
		t.Fatal(err)
	}
	replay, err := sealer.SealRevision(fixture.descriptor)
	if err != nil || !bytes.Equal(first, replay) {
		t.Fatalf("seal-once replay failed after key budget was consumed: equal=%v err=%v", bytes.Equal(first, replay), err)
	}
	nextRevision := fixedRecordID[content.FileRevision](4)
	next, err := content.NewFileRevisionDescriptor(
		fixture.share, fixture.file, nextRevision, fixture.descriptor.Geometry(), catalog.ModifiedTime{},
	)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := sealer.SealRevision(next); !errors.Is(err, ErrSealLimit) {
		t.Fatalf("file-object key seal limit error = %v", err)
	}

	broken := fixture.sealer(t, bytes.NewReader(nil), 1)
	if _, err := broken.SealRevision(fixture.descriptor); !errors.Is(err, ErrNonceSource) {
		t.Fatalf("revision nonce error = %v", err)
	}
	if uses, exists := broken.fileSealUses[fixture.file]; exists || uses != 0 {
		t.Fatalf("failed revision seal retained accounting: exists=%v uses=%d", exists, uses)
	}
	broken.nonceSource = bytes.NewReader(bytes.Repeat([]byte{4}, ObjectNonceBytes))
	if _, err := broken.SealRevision(fixture.descriptor); err != nil {
		t.Fatalf("failed revision seal consumed key budget: %v", err)
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
	if _, err := NewSealer(SealerConfig{}); err == nil {
		t.Fatal("invalid sealer accepted")
	}
	if _, err := NewOpener(OpenerConfig{}); err == nil {
		t.Fatal("invalid opener accepted")
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
		func(fields map[uint64]any) { fields[4] = catalog.MaxFileSize + 1 },
		func(fields map[uint64]any) { fields[5] = nil; fields[6] = uint64(1) },
		func(fields map[uint64]any) { fields[6] = uint64(1) << 32 },
		func(fields map[uint64]any) { fields[7] = uint64(255) },
		func(fields map[uint64]any) { delete(fields, 7) },
	}
	for index, mutate := range tests {
		fields := make(map[uint64]any, len(base))
		for key, value := range base {
			fields[key] = value
		}
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
		for key, value := range base {
			fields[key] = value
		}
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

func TestObjectBoundaryErrorsDoNotConsumeOrCrossIdentity(t *testing.T) {
	fixture := newRecordFixture(t, 2)
	sealer := fixture.sealer(t, bytes.NewReader(bytes.Repeat([]byte{1}, ObjectNonceBytes*4)), 4)
	otherShare := fixedRecordID[catalog.ShareInstance](9)
	otherDescriptor, _ := content.NewFileRevisionDescriptor(
		otherShare, fixture.file, fixture.revision, fixture.descriptor.Geometry(), catalog.ModifiedTime{},
	)
	if _, err := sealer.SealRevision(otherDescriptor); !errors.Is(err, ErrObjectIdentity) {
		t.Fatalf("cross-share revision error=%v", err)
	}
	otherRecord, _ := NewBlockRecord(otherDescriptor, 0, []byte{1, 2})
	if _, err := sealer.SealBlock(otherRecord); !errors.Is(err, ErrObjectIdentity) {
		t.Fatalf("cross-share block error=%v", err)
	}
	if _, err := fixture.opener(t).OpenRevision(fixture.file, catalog.MinChunkSize, make([]byte, MaxBlockRecordObjectBytes+1)); !errors.Is(err, ErrObjectTooLarge) {
		t.Fatalf("oversized object error=%v", err)
	}
	binding, _ := senderobject.NewRevisionBinding(fixture.share.Bytes(), fixture.file.Bytes())
	if _, err := senderobject.Seal(
		binding, make([]byte, 7), fixture.privateKey, make([]byte, ObjectNonceBytes), []byte{1},
	); !errors.Is(err, senderobject.ErrKey) {
		t.Fatalf("invalid AES key error=%v", err)
	}
	key := segmentSealKey{file: fixture.file, revision: fixture.revision}
	if !sealer.reserveSegmentSeal(key) || !sealer.reserveSegmentSeal(key) {
		t.Fatal("test seal reservations failed")
	}
	sealer.releaseSegmentSeal(key)
	sealer.releaseSegmentSeal(key)
}

func TestSenderObjectErrorsMapToRecordFailureCategories(t *testing.T) {
	sentinel := errors.New("unclassified sender object failure")
	tests := []struct {
		name string
		in   error
		want error
	}{
		{name: "nil", in: nil, want: nil},
		{name: "too large", in: senderobject.ErrTooLarge, want: ErrObjectTooLarge},
		{name: "malformed", in: senderobject.ErrMalformed, want: ErrObjectMalformed},
		{name: "signature", in: senderobject.ErrSignature, want: ErrObjectSignature},
		{name: "authentication", in: senderobject.ErrAuth, want: ErrObjectAuth},
		{name: "binding", in: senderobject.ErrBinding, want: ErrObjectIdentity},
		{name: "unclassified", in: sentinel, want: sentinel},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			got := mapSenderObjectError(test.in)
			if test.want == nil {
				if got != nil {
					t.Fatalf("nil sender-object error mapped to %v", got)
				}
				return
			}
			if !errors.Is(got, test.want) || !errors.Is(got, test.in) {
				t.Fatalf("mapped error=%v; want category=%v cause=%v", got, test.want, test.in)
			}
		})
	}
}
