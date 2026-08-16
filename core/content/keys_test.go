package content

import (
	"bytes"
	"errors"
	"testing"

	"github.com/windshare/windshare/core/catalog"
)

func TestDerivedKeyLifecycleAndNilSafe(t *testing.T) {
	var nilKey *DerivedKey
	nilKey.Destroy()

	key := &DerivedKey{value: [DerivedKeyBytes]byte{1, 2, 3, 4}}
	bytesCopy := key.Bytes()
	if !bytes.Equal(bytesCopy, []byte{1, 2, 3, 4, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0}) {
		t.Fatalf("unexpected key bytes: %v", bytesCopy)
	}
	key.Destroy()
	if !bytes.Equal(key.Bytes(), make([]byte, DerivedKeyBytes)) {
		t.Fatalf("key not zeroed after destroy: %v", key.Bytes())
	}
}

func TestKeyTreeLifecycleAndValidation(t *testing.T) {
	secret := bytes.Repeat([]byte{0x42}, ReadSecretBytes)
	share := catalogID[catalog.ShareInstance](10)
	fileID := catalogID[catalog.FileID](20)
	revision := contentID[FileRevision](30)
	pkHash := bytes.Repeat([]byte{0x55}, PKHashBytes)

	// NewKeyTree validation
	if _, err := NewKeyTree(secret[:10], share); err == nil {
		t.Fatal("expected error for short read secret")
	}
	if _, err := NewKeyTree(secret, catalog.ShareInstance{}); err == nil {
		t.Fatal("expected error for zero share instance")
	}

	tree, err := NewKeyTree(secret, share)
	if err != nil {
		t.Fatalf("failed to create KeyTree: %v", err)
	}

	// Nil receiver validation
	var nilTree *KeyTree
	nilTree.Destroy()

	if _, err := nilTree.DescriptorKey(pkHash); err == nil {
		t.Fatal("expected error for nil KeyTree DescriptorKey")
	}
	if _, err := nilTree.CatalogKey(); err == nil {
		t.Fatal("expected error for nil KeyTree CatalogKey")
	}
	if _, err := nilTree.SessionAuthKey(); err == nil {
		t.Fatal("expected error for nil KeyTree SessionAuthKey")
	}
	if _, err := nilTree.FileObjectKey(fileID); err == nil {
		t.Fatal("expected error for nil KeyTree FileObjectKey")
	}
	if _, err := nilTree.RevisionKey(fileID, revision); err == nil {
		t.Fatal("expected error for nil KeyTree RevisionKey")
	}
	if _, err := nilTree.FileSegmentKey(fileID, revision, 0, catalog.MinChunkSize); err == nil {
		t.Fatal("expected error for nil KeyTree FileSegmentKey")
	}

	// Bad input arguments
	if _, err := tree.DescriptorKey(pkHash[:10]); err == nil {
		t.Fatal("expected error for short pkHash")
	}
	if _, err := tree.FileObjectKey(catalog.FileID{}); err == nil {
		t.Fatal("expected error for zero FileID")
	}
	if _, err := tree.RevisionKey(catalog.FileID{}, revision); err == nil {
		t.Fatal("expected error for zero FileID in RevisionKey")
	}
	if _, err := tree.RevisionKey(fileID, FileRevision{}); err == nil {
		t.Fatal("expected error for zero FileRevision in RevisionKey")
	}
	if _, err := tree.FileSegmentKey(catalog.FileID{}, revision, 0, catalog.MinChunkSize); err == nil {
		t.Fatal("expected error for zero FileID in FileSegmentKey")
	}
	if _, err := tree.FileSegmentKey(fileID, FileRevision{}, 0, catalog.MinChunkSize); err == nil {
		t.Fatal("expected error for zero FileRevision in FileSegmentKey")
	}
	if _, err := tree.FileSegmentKey(fileID, revision, 0, 3); err == nil {
		t.Fatal("expected error for invalid chunkSize in FileSegmentKey")
	}

	// Successful derivations
	dk, err := tree.DescriptorKey(pkHash)
	if err != nil {
		t.Fatalf("DescriptorKey error: %v", err)
	}
	dk.Destroy()

	ck, err := tree.CatalogKey()
	if err != nil {
		t.Fatalf("CatalogKey error: %v", err)
	}
	ck.Destroy()

	sk, err := tree.SessionAuthKey()
	if err != nil {
		t.Fatalf("SessionAuthKey error: %v", err)
	}
	sk.Destroy()

	fok, err := tree.FileObjectKey(fileID)
	if err != nil {
		t.Fatalf("FileObjectKey error: %v", err)
	}
	fok.Destroy()

	rk, err := tree.RevisionKey(fileID, revision)
	if err != nil {
		t.Fatalf("RevisionKey error: %v", err)
	}
	rk.Destroy()

	fsk, err := tree.FileSegmentKey(fileID, revision, 0, catalog.MinChunkSize)
	if err != nil {
		t.Fatalf("FileSegmentKey error: %v", err)
	}
	fsk.Destroy()

	// Destroy and test error handling on destroyed tree
	tree.Destroy()

	if _, err := tree.DescriptorKey(pkHash); !errors.Is(err, ErrKeyTreeDestroyed) {
		t.Fatalf("expected ErrKeyTreeDestroyed for DescriptorKey, got %v", err)
	}
	if _, err := tree.CatalogKey(); !errors.Is(err, ErrKeyTreeDestroyed) {
		t.Fatalf("expected ErrKeyTreeDestroyed for CatalogKey, got %v", err)
	}
	if _, err := tree.SessionAuthKey(); !errors.Is(err, ErrKeyTreeDestroyed) {
		t.Fatalf("expected ErrKeyTreeDestroyed for SessionAuthKey, got %v", err)
	}
	if _, err := tree.FileObjectKey(fileID); !errors.Is(err, ErrKeyTreeDestroyed) {
		t.Fatalf("expected ErrKeyTreeDestroyed for FileObjectKey, got %v", err)
	}
	if _, err := tree.RevisionKey(fileID, revision); !errors.Is(err, ErrKeyTreeDestroyed) {
		t.Fatalf("expected ErrKeyTreeDestroyed for RevisionKey, got %v", err)
	}
	if _, err := tree.FileSegmentKey(fileID, revision, 0, catalog.MinChunkSize); !errors.Is(err, ErrKeyTreeDestroyed) {
		t.Fatalf("expected ErrKeyTreeDestroyed for FileSegmentKey, got %v", err)
	}
}

func TestSegmentForBlockValidation(t *testing.T) {
	// Chunk size smaller than minimum
	if _, err := SegmentForBlock(0, catalog.MinChunkSize-1); !errors.Is(err, ErrInvalidGeometry) {
		t.Fatalf("expected ErrInvalidGeometry for chunkSize < MinChunkSize, got %v", err)
	}
	// Chunk size larger than maximum
	if _, err := SegmentForBlock(0, catalog.MaxChunkSize+1); !errors.Is(err, ErrInvalidGeometry) {
		t.Fatalf("expected ErrInvalidGeometry for chunkSize > MaxChunkSize, got %v", err)
	}
	// Non-power of 2
	if _, err := SegmentForBlock(0, catalog.MinChunkSize+1); !errors.Is(err, ErrInvalidGeometry) {
		t.Fatalf("expected ErrInvalidGeometry for non-power-of-2 chunkSize, got %v", err)
	}

	// Valid block index to segment index
	seg, err := SegmentForBlock(100, catalog.MinChunkSize)
	if err != nil {
		t.Fatalf("unexpected error for SegmentForBlock: %v", err)
	}
	if seg != 0 {
		t.Fatalf("expected segment 0, got %d", seg)
	}
}

func TestOwnDerivedKeyEdgeCases(t *testing.T) {
	wantErr := errors.New("underlying error")
	if _, err := ownDerivedKey(nil, wantErr); !errors.Is(err, wantErr) {
		t.Fatalf("ownDerivedKey error = %v, want %v", err, wantErr)
	}
	if _, err := ownDerivedKey([]byte{1, 2, 3}, nil); err == nil {
		t.Fatal("ownDerivedKey accepted short slice")
	}
}
