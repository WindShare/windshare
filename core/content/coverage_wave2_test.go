package content

import (
	"bytes"
	"context"
	"errors"
	"testing"

	"github.com/windshare/windshare/core/catalog"
)

func TestContentWave2GeometryAndKeyTreeEdges(t *testing.T) {
	geometry, err := NewFileGeometry(2*uint64(catalog.MinChunkSize), catalog.MinChunkSize)
	if err != nil {
		t.Fatal(err)
	}
	if length, err := geometry.BlockPlainLength(1); err != nil || length != catalog.MinChunkSize {
		t.Fatalf("exact-tail block length = %d, %v", length, err)
	}
	if _, err := geometry.BlockOffset(geometry.BlockCount()); !errors.Is(err, ErrBlockOutOfRange) {
		t.Fatalf("block offset boundary = %v", err)
	}
	if _, err := geometry.BlocksForRanges(mustRangeSet(t, []Range{{Offset: 0, End: geometry.ExactSize() + 1}})); !errors.Is(err, ErrBlockOutOfRange) {
		t.Fatalf("range beyond geometry = %v", err)
	}
	wide, err := NewFileGeometry(uint64(MaxRequestedBlockIndices+1)*uint64(catalog.MinChunkSize), catalog.MinChunkSize)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := wide.BlocksForRanges(mustRangeSet(t, []Range{{Offset: 0, End: wide.ExactSize()}})); !errors.Is(err, ErrBlockRequestLimit) {
		t.Fatalf("block request limit = %v", err)
	}

	var nilTree *KeyTree
	if _, err := nilTree.CatalogKey(); err == nil {
		t.Fatal("nil key tree catalog derivation succeeded")
	}
	if _, err := nilTree.DescriptorKey(nil); err == nil {
		t.Fatal("nil key tree descriptor derivation succeeded")
	}
	if _, err := nilTree.FileObjectKey(catalog.FileID{}); err == nil {
		t.Fatal("nil key tree file derivation succeeded")
	}
	if _, err := nilTree.RevisionKey(catalog.FileID{}, FileRevision{}); err == nil {
		t.Fatal("nil key tree revision derivation succeeded")
	}
	if _, err := nilTree.FileSegmentKey(catalog.FileID{}, FileRevision{}, 0, catalog.MinChunkSize); err == nil {
		t.Fatal("nil key tree segment derivation succeeded")
	}
	if _, err := SegmentForBlock(0, 0); !errors.Is(err, ErrInvalidGeometry) {
		t.Fatalf("invalid segment chunk = %v", err)
	}

	secret := bytes.Repeat([]byte{0x31}, ReadSecretBytes)
	tree, err := NewKeyTree(secret, contentWave2ShareID(1))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := tree.DescriptorKey(nil); err == nil {
		t.Fatal("short descriptor context accepted")
	}
	var file catalog.FileID
	file[0] = 2
	var revision FileRevision
	revision[0] = 3
	if _, err := tree.CatalogKey(); err != nil {
		t.Fatal(err)
	}
	if _, err := tree.SessionAuthKey(); err != nil {
		t.Fatal(err)
	}
	if _, err := tree.RevisionKey(file, FileRevision{}); err == nil {
		t.Fatal("zero revision context accepted")
	}
	if _, err := tree.FileSegmentKey(file, revision, 0, 1000); !errors.Is(err, ErrInvalidGeometry) {
		t.Fatalf("invalid segment geometry = %v", err)
	}
	derived, err := tree.FileSegmentKey(file, revision, 0, catalog.MinChunkSize)
	if err != nil || slicesAllZero(derived.Bytes()) {
		t.Fatalf("segment key = %v", err)
	}
	tree.Destroy()
	if _, err := tree.SessionAuthKey(); !errors.Is(err, ErrKeyTreeDestroyed) {
		t.Fatalf("destroyed key tree = %v", err)
	}
	var key DerivedKey
	key.Destroy()

	raw := bytes.Repeat([]byte{0x42}, DerivedKeyBytes)
	owned, err := ownDerivedKey(raw, nil)
	if err != nil || slicesAllZero(owned.Bytes()) || slicesAllZero(raw) == false {
		t.Fatalf("owned key = %v, raw cleared=%t", err, slicesAllZero(raw))
	}
	if _, err := ownDerivedKey([]byte{1}, nil); err == nil {
		t.Fatal("short derived key accepted")
	}
	if _, err := ownDerivedKey(nil, context.Canceled); !errors.Is(err, context.Canceled) {
		t.Fatalf("derived key source error = %v", err)
	}
}

func TestContentWave2QuotaReservationEdges(t *testing.T) {
	if _, err := reserveQuotaAccounts(nil, QuotaUsage{ActiveLeases: 1}); err == nil {
		t.Fatal("empty quota account list accepted")
	}
	account, err := NewQuotaAccount("wave2", QuotaLimits{StableHandles: 2, ActiveLeases: 2})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := account.reserve(QuotaUsage{StableHandles: 3}); !errors.Is(err, ErrQuotaExceeded) {
		t.Fatalf("quota overage = %v", err)
	}
	reservation, err := reserveQuotaAccounts([]*QuotaAccount{account}, QuotaUsage{StableHandles: 1, ActiveLeases: 1})
	if err != nil {
		t.Fatal(err)
	}
	reservation.Release()
	reservation.Release()
	if got := account.Snapshot().Used; got != (QuotaUsage{}) {
		t.Fatalf("idempotent release usage = %+v", got)
	}
	if _, err := reserveQuotaAccounts([]*QuotaAccount{account, nil}, QuotaUsage{ActiveLeases: 1}); err == nil {
		t.Fatal("nil account in hierarchy accepted")
	}
}

func mustRangeSet(t *testing.T, ranges []Range) RangeSet {
	t.Helper()
	set, err := NewRangeSet(ranges)
	if err != nil {
		t.Fatal(err)
	}
	return set
}

func contentWave2ShareID(value byte) catalog.ShareInstance {
	var id catalog.ShareInstance
	id[0] = value
	return id
}

func slicesAllZero(raw []byte) bool {
	for _, value := range raw {
		if value != 0 {
			return false
		}
	}
	return true
}
