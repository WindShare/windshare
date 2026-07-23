package content

import (
	"bytes"
	"crypto/hkdf"
	"crypto/sha256"
	"encoding/binary"
	"errors"
	"testing"
	"time"

	"github.com/windshare/windshare/core/catalog"
)

func TestRevisionLeaseRequiresBoundIdentitiesAndRelativeTiming(t *testing.T) {
	geometry, err := NewFileGeometry(7, catalog.MinChunkSize)
	if err != nil {
		t.Fatal(err)
	}
	modified, err := catalog.NewModifiedTime(1, 2, catalog.TimePrecisionNanoseconds)
	if err != nil {
		t.Fatal(err)
	}
	descriptor, err := NewFileRevisionDescriptor(
		catalogID[catalog.ShareInstance](1),
		catalogID[catalog.FileID](2),
		contentID[FileRevision](3),
		geometry,
		modified,
	)
	if err != nil {
		t.Fatal(err)
	}
	lease, err := NewRevisionLease(contentID[LeaseID](4), descriptor, time.Minute, 30*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	if lease.ID().IsZero() || lease.Descriptor().FileID().IsZero() || lease.TTL() != time.Minute || lease.RenewAfter() != 30*time.Second {
		t.Fatalf("lease = %+v", lease)
	}
	if _, err := NewRevisionLease(LeaseID{}, descriptor, time.Minute, 0); err == nil {
		t.Fatal("zero lease identity was accepted")
	}
	if _, err := NewRevisionLease(contentID[LeaseID](4), descriptor, time.Minute, time.Minute+time.Second); err == nil {
		t.Fatal("renewal after expiry was accepted")
	}
}

func contentID[T ~[IdentityBytes]byte](value byte) T {
	var id T
	id[0] = value
	return id
}

func catalogID[T ~[catalog.IdentityBytes]byte](value byte) T {
	var id T
	id[0] = value
	return id
}

func TestFileGeometryDerivesLocalBlocksWithoutEncodedBlockCount(t *testing.T) {
	zero, err := NewFileGeometry(0, catalog.DefaultChunkSize)
	if err != nil || zero.BlockCount() != 0 {
		t.Fatalf("zero geometry = %+v, %v", zero, err)
	}
	geometry, err := NewFileGeometry(uint64(catalog.DefaultChunkSize)+7, catalog.DefaultChunkSize)
	if err != nil {
		t.Fatal(err)
	}
	if geometry.BlockCount() != 2 {
		t.Fatalf("block count = %d", geometry.BlockCount())
	}
	if length, err := geometry.BlockPlainLength(1); err != nil || length != 7 {
		t.Fatalf("tail length = %d, %v", length, err)
	}
	if _, err := geometry.BlockPlainLength(2); !errors.Is(err, ErrBlockOutOfRange) {
		t.Fatalf("out-of-range error = %v", err)
	}
	for _, input := range []struct {
		size  uint64
		chunk uint32
	}{{catalog.MaxFileSize + 1, catalog.DefaultChunkSize}, {1, 1000}} {
		if _, err := NewFileGeometry(input.size, input.chunk); err == nil {
			t.Fatalf("invalid geometry %+v was accepted", input)
		}
	}
}

func TestRangeSetIsStrictCanonicalAndMapsOnlyIntersectingBlocks(t *testing.T) {
	ranges, err := NewRangeSet([]Range{{Offset: 2, End: 4}, {Offset: 2 * uint64(catalog.MinChunkSize), End: 2*uint64(catalog.MinChunkSize) + 2}})
	if err != nil {
		t.Fatal(err)
	}
	geometry, _ := NewFileGeometry(2*uint64(catalog.MinChunkSize)+4, catalog.MinChunkSize)
	blocks, err := geometry.BlocksForRanges(ranges)
	if err != nil {
		t.Fatal(err)
	}
	if want := []uint64{0, 2}; !slicesEqual(blocks, want) {
		t.Fatalf("blocks = %v, want %v", blocks, want)
	}
	if ranges.Len() != 2 {
		t.Fatalf("range count = %d", ranges.Len())
	}
	for _, invalid := range [][]Range{
		{{Offset: 4, End: 4}},
		{{Offset: 8, End: 10}, {Offset: 2, End: 4}},
		{{Offset: 2, End: 4}, {Offset: 4, End: 8}},
	} {
		if _, err := NewRangeSet(invalid); !errors.Is(err, ErrNonCanonicalRange) {
			t.Fatalf("range error = %v for %+v", err, invalid)
		}
	}
	returned := ranges.Ranges()
	returned[0] = Range{}
	if ranges.Ranges()[0].Offset != 2 {
		t.Fatal("range set exposed mutable storage")
	}
}

func slicesEqual(left, right []uint64) bool {
	if len(left) != len(right) {
		return false
	}
	for index := range left {
		if left[index] != right[index] {
			return false
		}
	}
	return true
}

func TestBlockRefBindsFileRevisionAndLocalIndex(t *testing.T) {
	geometry, _ := NewFileGeometry(2*uint64(catalog.MinChunkSize)+1, catalog.MinChunkSize)
	file := catalogID[catalog.FileID](1)
	revision := contentID[FileRevision](2)
	ref, err := NewBlockRef(file, revision, 2, geometry)
	if err != nil {
		t.Fatal(err)
	}
	if ref.FileID() != file || ref.FileRevision() != revision || ref.LocalBlockIndex() != 2 {
		t.Fatalf("block ref = %+v", ref)
	}
	if _, err := NewBlockRef(file, revision, 3, geometry); !errors.Is(err, ErrBlockOutOfRange) {
		t.Fatalf("invalid ref error = %v", err)
	}
}

func TestKeyTreeUsesFrozenDomainSeparatedContexts(t *testing.T) {
	readSecret := bytes.Repeat([]byte{0x11}, ReadSecretBytes)
	share := catalogID[catalog.ShareInstance](1)
	file := catalogID[catalog.FileID](2)
	revision := contentID[FileRevision](3)
	tree, err := NewKeyTree(readSecret, share)
	if err != nil {
		t.Fatal(err)
	}
	fileKey, err := tree.FileObjectKey(file)
	if err != nil {
		t.Fatal(err)
	}
	wantFileKey := directHKDF(t, readSecret, FileObjectKeyLabel, append(share.Bytes(), file.Bytes()...))
	if !bytes.Equal(fileKey.Bytes(), wantFileKey) {
		t.Fatalf("file key = %x, want %x", fileKey.Bytes(), wantFileKey)
	}
	revisionKey, _ := tree.RevisionKey(file, revision)
	wantRevisionKey := directHKDF(t, wantFileKey, RevisionKeyLabel, revision.Bytes())
	if !bytes.Equal(revisionKey.Bytes(), wantRevisionKey) {
		t.Fatal("revision derivation did not use the frozen hierarchy")
	}

	segment, err := SegmentForBlock((SegmentPlaintextBytes/uint64(catalog.DefaultChunkSize))*2-1, catalog.DefaultChunkSize)
	if err != nil || segment != 1 {
		t.Fatalf("segment = %d, %v", segment, err)
	}
	segmentKey, _ := tree.FileSegmentKey(file, revision, (SegmentPlaintextBytes/uint64(catalog.DefaultChunkSize))*2-1, catalog.DefaultChunkSize)
	var context [8]byte
	binary.BigEndian.PutUint64(context[:], 1)
	wantSegmentKey := directHKDF(t, wantRevisionKey, FileSegmentKeyLabel, context[:])
	if !bytes.Equal(segmentKey.Bytes(), wantSegmentKey) {
		t.Fatal("segment derivation did not bind its big-endian segment number")
	}

	otherRevision := contentID[FileRevision](4)
	otherKey, _ := tree.RevisionKey(file, otherRevision)
	if bytes.Equal(revisionKey.Bytes(), otherKey.Bytes()) {
		t.Fatal("different revision identity reused a key")
	}
}

func directHKDF(t *testing.T, secret []byte, label string, context []byte) []byte {
	t.Helper()
	info := append(append([]byte(label), 0), context...)
	key, err := hkdf.Key(sha256.New, secret, nil, string(info), DerivedKeyBytes)
	if err != nil {
		t.Fatal(err)
	}
	return key
}
