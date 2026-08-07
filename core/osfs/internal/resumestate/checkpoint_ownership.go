package resumestate

import (
	"bytes"
	"crypto/sha256"
	"fmt"

	"github.com/windshare/windshare/core/transfer"
)

// FileCheckpointOwnership is the small root marker a cleaner must verify before
// it is allowed to inspect old state.  It intentionally carries no file ranges
// or selection data: a marker proves namespace ownership, never resume authority.
type FileCheckpointOwnership struct {
	Marker       string
	Namespace    string
	BackendID    string
	RootIdentity FileCheckpointRootID
}

func NewFileCheckpointOwnership(backendID string, rootIdentity []byte) (FileCheckpointOwnership, error) {
	backend, err := transfer.NewOutputBackendID(backendID)
	if err != nil {
		return FileCheckpointOwnership{}, fmt.Errorf("%w: ownership backend", ErrFileCheckpointOwnership)
	}
	root, err := FileCheckpointRootIDFromBytes(rootIdentity)
	if err != nil {
		return FileCheckpointOwnership{}, err
	}
	return FileCheckpointOwnership{
		Marker: FileCheckpointOwnershipMarker, Namespace: FileCheckpointNamespace,
		BackendID: string(backend), RootIdentity: root,
	}, nil
}

func (ownership FileCheckpointOwnership) valid() error {
	if err := validateMarkerAndNamespace(ownership.Marker, ownership.Namespace); err != nil {
		return err
	}
	if _, err := transfer.NewOutputBackendID(ownership.BackendID); err != nil {
		return fmt.Errorf("%w: ownership backend", ErrFileCheckpointOwnership)
	}
	if ownership.RootIdentity.IsZero() {
		return fmt.Errorf("%w: ownership root", ErrFileCheckpointOwnership)
	}
	return nil
}

func (ownership FileCheckpointOwnership) CanonicalBytes() []byte {
	if ownership.valid() != nil {
		return nil
	}
	var encoded bytes.Buffer
	writeCheckpointString(&encoded, "windshare/file-checkpoint-ownership/v1")
	writeCheckpointString(&encoded, ownership.Marker)
	writeCheckpointString(&encoded, ownership.Namespace)
	writeCheckpointString(&encoded, ownership.BackendID)
	encoded.Write(ownership.RootIdentity[:])
	return encoded.Bytes()
}

func EncodeFileCheckpointOwnership(ownership FileCheckpointOwnership) ([]byte, error) {
	if err := ownership.valid(); err != nil {
		return nil, err
	}
	payload := ownership.CanonicalBytes()
	hash := sha256.Sum256(append([]byte(fileCheckpointChecksumDomain+"\x00"), payload...))
	return append(payload, hash[:]...), nil
}

func DecodeFileCheckpointOwnership(encoded []byte) (FileCheckpointOwnership, error) {
	if len(encoded) < sha256.Size+1 {
		return FileCheckpointOwnership{}, ErrFileCheckpointOwnership
	}
	payload, checksum := encoded[:len(encoded)-sha256.Size], encoded[len(encoded)-sha256.Size:]
	expected := sha256.Sum256(append([]byte(fileCheckpointChecksumDomain+"\x00"), payload...))
	if !bytes.Equal(checksum, expected[:]) {
		return FileCheckpointOwnership{}, ErrFileCheckpointChecksum
	}
	cursor := checkpointCursor{bytes: payload}
	domain, err := cursor.string(128)
	if err != nil || domain != "windshare/file-checkpoint-ownership/v1" {
		return FileCheckpointOwnership{}, ErrFileCheckpointOwnership
	}
	marker, err := cursor.string(maxCheckpointMarkerBytes)
	if err != nil {
		return FileCheckpointOwnership{}, err
	}
	namespace, err := cursor.string(maxCheckpointNamespace)
	if err != nil {
		return FileCheckpointOwnership{}, err
	}
	backend, err := cursor.string(maxCheckpointBackendBytes)
	if err != nil {
		return FileCheckpointOwnership{}, err
	}
	rootRaw, err := cursor.take(sha256.Size)
	if err != nil || cursor.off != len(payload) {
		return FileCheckpointOwnership{}, ErrFileCheckpointOwnership
	}
	root, err := FileCheckpointRootIDFromBytes(rootRaw)
	if err != nil {
		return FileCheckpointOwnership{}, err
	}
	ownership := FileCheckpointOwnership{Marker: marker, Namespace: namespace, BackendID: backend, RootIdentity: root}
	if err := ownership.valid(); err != nil {
		return FileCheckpointOwnership{}, err
	}
	if !bytes.Equal(ownership.CanonicalBytes(), payload) {
		return FileCheckpointOwnership{}, ErrFileCheckpointNonCanonical
	}
	return ownership, nil
}

// The fixed-size identities intentionally do not expose mutable slices.  A
// backend may derive these values from a native root/object identity, but it can
// never accidentally persist an inode number or a path spelling as the binding.
