package content

import (
	"bytes"
	"errors"
	"math"
	"sync"
	"testing"

	"github.com/windshare/windshare/core/catalog"
)

func TestContentIdentityParsingAndEquality(t *testing.T) {
	raw := bytes.Repeat([]byte{0x4a}, IdentityBytes)
	revision, err := FileRevisionFromBytes(raw)
	if err != nil {
		t.Fatal(err)
	}
	lease, err := LeaseIDFromBytes(raw)
	if err != nil {
		t.Fatal(err)
	}
	if !revision.Equal(FileRevision(lease)) || !lease.Equal(LeaseID(revision)) || !bytes.Equal(lease.Bytes(), raw) {
		t.Fatal("content identity parsing changed opaque bytes")
	}
	if _, err := FileRevisionFromBytes(raw[:IdentityBytes-1]); !errors.Is(err, ErrIdentityLength) {
		t.Fatalf("short revision error = %v", err)
	}
	if _, err := LeaseIDFromBytes(raw[:IdentityBytes-1]); !errors.Is(err, ErrIdentityLength) {
		t.Fatalf("short lease error = %v", err)
	}
}

func TestGeometryRejectsHostileRangesAndReferences(t *testing.T) {
	geometry, _ := NewFileGeometry(300*uint64(catalog.MinChunkSize), catalog.MinChunkSize)
	if _, err := geometry.BlockOffset(geometry.BlockCount()); !errors.Is(err, ErrBlockOutOfRange) {
		t.Fatalf("block offset error = %v", err)
	}
	if length, err := geometry.BlockPlainLength(geometry.BlockCount() - 1); err != nil || length != catalog.MinChunkSize {
		t.Fatalf("full tail length = %d, %v", length, err)
	}
	whole, _ := NewRangeSet([]Range{{Offset: 0, End: geometry.ExactSize()}})
	if _, err := geometry.BlocksForRanges(whole); !errors.Is(err, ErrBlockRequestLimit) {
		t.Fatalf("block request limit error = %v", err)
	}
	outside, _ := NewRangeSet([]Range{{Offset: geometry.ExactSize(), End: geometry.ExactSize() + 1}})
	if _, err := geometry.BlocksForRanges(outside); !errors.Is(err, ErrBlockOutOfRange) {
		t.Fatalf("outside range error = %v", err)
	}
	if (Range{Offset: 2, End: 9}).Length() != 7 {
		t.Fatal("range length is not half-open")
	}
	if _, err := NewBlockRef(catalog.FileID{}, contentID[FileRevision](1), 0, geometry); !errors.Is(err, ErrInvalidBlockRef) {
		t.Fatalf("zero file block ref = %v", err)
	}
}

func TestKeyTreeCoversEveryFrozenDomainAndRejectsInvalidContexts(t *testing.T) {
	secret := bytes.Repeat([]byte{0x22}, ReadSecretBytes)
	share := catalogID[catalog.ShareInstance](1)
	tree, _ := NewKeyTree(secret, share)
	pkHash := bytes.Repeat([]byte{0x33}, PKHashBytes)
	descriptor, err := tree.DescriptorKey(pkHash)
	if err != nil || !bytes.Equal(descriptor.Bytes(), directHKDF(t, secret, DescriptorKeyLabel, pkHash)) {
		t.Fatal("descriptor domain derivation mismatch")
	}
	catalogKey, err := tree.CatalogKey()
	if err != nil || !bytes.Equal(catalogKey.Bytes(), directHKDF(t, secret, CatalogKeyLabel, share.Bytes())) {
		t.Fatal("catalog domain derivation mismatch")
	}
	sessionKey, err := tree.SessionAuthKey()
	if err != nil || !bytes.Equal(sessionKey.Bytes(), directHKDF(t, secret, SessionAuthKeyLabel, share.Bytes())) {
		t.Fatal("session-auth domain derivation mismatch")
	}
	if _, err := tree.DescriptorKey(pkHash[:PKHashBytes-1]); err == nil {
		t.Fatal("short public-key hash was accepted")
	}
	if _, err := NewKeyTree(secret[:ReadSecretBytes-1], share); err == nil {
		t.Fatal("short read secret was accepted")
	}
	if _, err := (*KeyTree)(nil).CatalogKey(); err == nil {
		t.Fatal("nil catalog key tree was accepted")
	}
	if _, err := (*KeyTree)(nil).SessionAuthKey(); err == nil {
		t.Fatal("nil session key tree was accepted")
	}
	if _, err := tree.FileObjectKey(catalog.FileID{}); err == nil {
		t.Fatal("zero file key context was accepted")
	}
	if _, err := tree.RevisionKey(catalogID[catalog.FileID](1), FileRevision{}); err == nil {
		t.Fatal("zero revision key context was accepted")
	}
	if _, err := SegmentForBlock(0, 1000); err == nil {
		t.Fatal("invalid segment geometry was accepted")
	}
	var nilKey *DerivedKey
	nilKey.Destroy()
	tree.Destroy()
	if !bytes.Equal(tree.readSecret[:], make([]byte, ReadSecretBytes)) {
		t.Fatal("key tree destroy retained the read secret")
	}
	if _, err := tree.CatalogKey(); !errors.Is(err, ErrKeyTreeDestroyed) {
		t.Fatalf("destroyed key tree derived a catalog key: %v", err)
	}
	if _, err := tree.FileSegmentKey(catalogID[catalog.FileID](1), contentID[FileRevision](1), 0, catalog.MinChunkSize); !errors.Is(err, ErrKeyTreeDestroyed) {
		t.Fatalf("destroyed key tree derived a segment key: %v", err)
	}
}

func TestKeyTreeDestroySerializesWithConcurrentDerivation(t *testing.T) {
	tree, err := NewKeyTree(bytes.Repeat([]byte{0x44}, ReadSecretBytes), catalogID[catalog.ShareInstance](1))
	if err != nil {
		t.Fatal(err)
	}
	start := make(chan struct{})
	errorsOut := make(chan error, 16)
	var workers sync.WaitGroup
	for range 16 {
		workers.Add(1)
		go func() {
			defer workers.Done()
			<-start
			for range 32 {
				_, err := tree.FileObjectKey(catalogID[catalog.FileID](1))
				if err != nil && !errors.Is(err, ErrKeyTreeDestroyed) {
					errorsOut <- err
					return
				}
			}
		}()
	}
	close(start)
	tree.Destroy()
	workers.Wait()
	close(errorsOut)
	for err := range errorsOut {
		t.Fatalf("concurrent key derivation failed: %v", err)
	}
}

func TestQuotaHierarchyRejectsInvalidAndOverflowingAdmissions(t *testing.T) {
	if DefaultSessionQuotaLimits().ActiveLeases != DefaultSessionActiveLeases ||
		DefaultShareQuotaLimits().StableHandles != DefaultShareStableHandles ||
		DefaultProcessQuotaLimits().ActiveLeases != DefaultProcessActiveLeases {
		t.Fatal("content default quota profile drifted from the frozen limits")
	}
	if _, err := NewQuotaAccount("", QuotaLimits{StableHandles: 1, ActiveLeases: 1}); err == nil {
		t.Fatal("unnamed quota was accepted")
	}
	process := generousQuota(t, "process")
	share := generousQuota(t, "share")
	session := generousQuota(t, "session")
	if _, err := ReserveQuota(QuotaHierarchy{}, QuotaUsage{ActiveLeases: 1}); err == nil {
		t.Fatal("nil quota hierarchy was accepted")
	}
	if _, err := ReserveQuota(QuotaHierarchy{Process: process, Share: process, Session: session}, QuotaUsage{ActiveLeases: 1}); err == nil {
		t.Fatal("aliased quota hierarchy was accepted")
	}
	large, _ := NewQuotaAccount("large", QuotaLimits{StableHandles: math.MaxUint64, ActiveLeases: math.MaxUint64})
	reservation, err := large.reserve(QuotaUsage{ActiveLeases: math.MaxUint64})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := large.reserve(QuotaUsage{ActiveLeases: 1}); !errors.Is(err, ErrQuotaExceeded) {
		t.Fatalf("overflow quota error = %v", err)
	}
	reservation.release()
	if (*QuotaAccount)(nil).Name() != "" || (*QuotaAccount)(nil).Snapshot() != (QuotaSnapshot{}) {
		t.Fatal("nil quota inspection is not safe")
	}
	var nilReservation *QuotaReservation
	nilReservation.Release()
	if process.Name() != "process" || share.Name() != "share" {
		t.Fatal("quota account names changed")
	}
}

func TestRevisionDescriptorRejectsMissingIdentityAndExposesOnlySemanticFields(t *testing.T) {
	geometry, _ := NewFileGeometry(7, catalog.MinChunkSize)
	if _, err := NewFileRevisionDescriptor(catalog.ShareInstance{}, catalogID[catalog.FileID](1), contentID[FileRevision](1), geometry, catalog.ModifiedTime{}); err == nil {
		t.Fatal("identity-free descriptor was accepted")
	}
	descriptor, err := NewFileRevisionDescriptor(catalogID[catalog.ShareInstance](1), catalogID[catalog.FileID](2), contentID[FileRevision](3), geometry, catalog.ModifiedTime{})
	if err != nil {
		t.Fatal(err)
	}
	if descriptor.ShareInstance().IsZero() || descriptor.ExactSize() != 7 || descriptor.ModifiedTime().Present() {
		t.Fatalf("descriptor accessors = %+v", descriptor)
	}
}
