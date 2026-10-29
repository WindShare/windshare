package outputportable

import (
	"bytes"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"io"
	"io/fs"

	"github.com/windshare/windshare/osfs/internal/outputcap"
)

const (
	directoryIdentityMagic       = "windshare/directory-witness/v1\n"
	directoryIdentityBytes       = sha256.Size
	directoryIdentityRecordBytes = len(directoryIdentityMagic) + 2*directoryIdentityBytes
)

// A random, task-owned marker survives both remount-derived inode changes and
// metadata updates. Its name is derived from the retained claim, never a scan.
func directoryIdentityName(claim []byte) (string, error) {
	if len(claim) != directoryIdentityBytes || bytes.Equal(claim, make([]byte, directoryIdentityBytes)) {
		return "", outputcap.ErrUnsafeNamespace
	}
	return ".wsresume-" + hex.EncodeToString(claim) + ".identity", nil
}

func encodeDirectoryIdentity(claim []byte) []byte {
	encoded := append([]byte(directoryIdentityMagic), claim...)
	checksum := sha256.Sum256(encoded)
	return append(encoded, checksum[:]...)
}

func decodeDirectoryIdentity(encoded []byte) ([]byte, error) {
	if len(encoded) != directoryIdentityRecordBytes || !bytes.HasPrefix(encoded, []byte(directoryIdentityMagic)) {
		return nil, outputcap.ErrUnsafeNamespace
	}
	payload := encoded[:len(encoded)-sha256.Size]
	checksum := sha256.Sum256(payload)
	if !bytes.Equal(encoded[len(payload):], checksum[:]) {
		return nil, outputcap.ErrUnsafeNamespace
	}
	claim := append([]byte(nil), payload[len(directoryIdentityMagic):]...)
	if _, err := directoryIdentityName(claim); err != nil {
		return nil, err
	}
	return claim, nil
}

func readDirectoryIdentity(file outputcap.ObservedFile) ([]byte, error) {
	size, err := file.Size()
	if err != nil || size != uint64(directoryIdentityRecordBytes) {
		return nil, errors.Join(outputcap.ErrUnsafeNamespace, err)
	}
	encoded := make([]byte, directoryIdentityRecordBytes)
	if _, err := file.ReadAt(encoded, 0); err != nil {
		return nil, err
	}
	return decodeDirectoryIdentity(encoded)
}

func (d *directory) createDirectoryIdentity() (outputcap.MutableFile, []byte, error) {
	claim := make([]byte, directoryIdentityBytes)
	if _, err := io.ReadFull(rand.Reader, claim); err != nil {
		return nil, nil, err
	}
	name, err := directoryIdentityName(claim)
	if err != nil {
		return nil, nil, err
	}
	file, err := d.CreateFile(name, true, int64(directoryIdentityRecordBytes))
	if err != nil {
		return nil, nil, err
	}
	if _, err := file.WriteAt(encodeDirectoryIdentity(claim), 0); err != nil {
		return nil, nil, errors.Join(err, d.RemoveFile(name, file), file.Close())
	}
	if err := errors.Join(file.Sync(), d.Sync()); err != nil {
		return nil, nil, errors.Join(err, d.RemoveFile(name, file), file.Close())
	}
	return file, claim, nil
}

func (d *directory) CreateRecoveryIdentity() ([]byte, error) {
	file, claim, err := d.createDirectoryIdentity()
	if err != nil {
		return nil, err
	}
	return claim, file.Close()
}

func (d *directory) MatchesRecoveryIdentity(expected []byte) (bool, error) {
	name, err := directoryIdentityName(expected)
	if err != nil {
		return false, err
	}
	file, err := d.OpenObservedFile(name, true)
	if errors.Is(err, fs.ErrNotExist) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	actual, readErr := readDirectoryIdentity(file)
	return readErr == nil && bytes.Equal(actual, expected), errors.Join(readErr, file.Close())
}

func (d *directory) RetireRecoveryIdentity(expected []byte) error {
	name, err := directoryIdentityName(expected)
	if err != nil {
		return err
	}
	return d.retireDirectoryIdentity(name, expected)
}

func (d *directory) retireDirectoryIdentity(name string, expected []byte) error {
	file, err := d.OpenObservedFile(name, true)
	if errors.Is(err, fs.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	actual, readErr := readDirectoryIdentity(file)
	if readErr != nil || !bytes.Equal(actual, expected) {
		return errors.Join(outputcap.ErrUnsafeNamespace, readErr, file.Close())
	}
	info, statErr := d.handle.Stat()
	if statErr != nil {
		return errors.Join(statErr, file.Close())
	}
	removeErr := d.RemoveFile(name, file)
	if removeErr == nil {
		removeErr = errors.Join(setDirectoryTimes(d, info.ModTime()), d.Sync())
	}
	return errors.Join(removeErr, file.Close())
}
