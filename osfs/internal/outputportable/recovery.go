package outputportable

import (
	"crypto/rand"
	"errors"
	"io"

	"github.com/windshare/windshare/osfs/internal/outputcap"
	"github.com/windshare/windshare/osfs/internal/recoverywitness"
)

func (f *file) CreateRecoveryWitness() ([]byte, error) {
	size, err := f.physicalSize()
	if err != nil {
		return nil, err
	}
	witness, err := recoverywitness.New(size, rand.Reader)
	if err != nil {
		return nil, err
	}
	if _, err := f.handle.WriteAt(witness.Trailer(), int64(size)); err != nil {
		return nil, err
	}
	// The object token must survive before a checkpoint can claim any ranges.
	if err := f.handle.Sync(); err != nil {
		return nil, err
	}
	f.recovery = &witness
	return witness.Encode(), nil
}

func (f *file) BindRecoveryWitness(encoded []byte) (bool, error) {
	witness, err := recoverywitness.Decode(encoded)
	if err != nil {
		return false, errors.Join(outputcap.ErrUnsafeNamespace, err)
	}
	size, err := f.physicalSize()
	if err != nil {
		return false, err
	}
	matches, err := witness.MatchesTrailer(f.handle, size)
	if err != nil {
		return false, err
	}
	if matches {
		f.recovery = &witness
		return true, nil
	}
	if size != witness.Size() || !witness.Publishing() {
		return false, nil
	}
	fingerprint, err := recoverywitness.PublicationFingerprint(f.handle, size)
	if err != nil || fingerprint != witness.Fingerprint() {
		return false, err
	}
	// Publication reconciliation permits finishing delivery, not erasing an
	// arbitrary file with matching samples. Cleanup requires the embedded nonce.
	f.recovery = &witness
	return true, nil
}

func (f *file) MatchesPublishedRecoveryWitness(encoded []byte) (bool, error) {
	witness, err := recoverywitness.Decode(encoded)
	if err != nil {
		return false, errors.Join(outputcap.ErrUnsafeNamespace, err)
	}
	size, err := f.physicalSize()
	if err != nil || !witness.Publishing() || size != witness.Size() {
		return false, err
	}
	fingerprint, err := recoverywitness.PublicationFingerprint(f.handle, size)
	return err == nil && fingerprint == witness.Fingerprint(), err
}

func (f *file) RecoveryRemovalAllowed(encoded []byte) (bool, error) {
	witness, err := recoverywitness.Decode(encoded)
	if err != nil {
		return false, errors.Join(outputcap.ErrUnsafeNamespace, err)
	}
	size, err := f.physicalSize()
	if err != nil {
		return false, err
	}
	return witness.MatchesTrailer(f.handle, size)
}

func (f *file) PrepareRecoveryPublication(encoded []byte) ([]byte, error) {
	matches, err := f.BindRecoveryWitness(encoded)
	if err != nil || !matches {
		return nil, errors.Join(outputcap.ErrUnsafeNamespace, err)
	}
	if f.recovery.Publishing() {
		return f.recovery.Encode(), nil
	}
	fingerprint, err := recoverywitness.PublicationFingerprint(f.handle, f.recovery.Size())
	if err != nil {
		return nil, err
	}
	witness := f.recovery.ForPublication(fingerprint)
	return witness.Encode(), nil
}

func (f *file) StripRecoveryWitness(encoded []byte) error {
	matches, err := f.BindRecoveryWitness(encoded)
	if err != nil || !matches || !f.recovery.Publishing() {
		return errors.Join(outputcap.ErrUnsafeNamespace, err)
	}
	info, err := f.handle.Stat()
	if err != nil {
		return err
	}
	if uint64(info.Size()) == f.recovery.Size() {
		return nil
	}
	if err := f.handle.Truncate(int64(f.recovery.Size())); err != nil {
		return err
	}
	f.strippedOwned = true
	// Truncating private ownership bytes must not change the source's published mtime.
	return errors.Join(setFileModifiedTime(f.handle, info.ModTime()), f.handle.Sync())
}

func (f *file) RestoreRecoveryWitness() error {
	if !f.strippedOwned {
		return nil
	}
	info, err := f.handle.Stat()
	if err != nil {
		return err
	}
	if uint64(info.Size()) != f.recovery.Size() {
		return outputcap.ErrUnsafeNamespace
	}
	if _, err := f.handle.WriteAt(f.recovery.Trailer(), int64(f.recovery.Size())); err != nil {
		return err
	}
	f.strippedOwned = false
	return errors.Join(setFileModifiedTime(f.handle, info.ModTime()), f.handle.Sync())
}

func (f *file) recoveryRead(p []byte, offset int64) (int, error) {
	if f.recovery == nil {
		return f.handle.ReadAt(p, offset)
	}
	if offset < 0 {
		return 0, outputcap.ErrUnsafeNamespace
	}
	size := f.recovery.Size()
	if uint64(offset) >= size {
		if len(p) == 0 {
			return 0, nil
		}
		return 0, io.EOF
	}
	limit := min(uint64(len(p)), size-uint64(offset))
	n, err := f.handle.ReadAt(p[:limit], offset)
	if err == nil && limit < uint64(len(p)) {
		err = io.EOF
	}
	return n, err
}
