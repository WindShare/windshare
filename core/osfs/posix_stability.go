//go:build linux || darwin

package osfs

import (
	"context"
	"crypto/subtle"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"math"
	"os"

	"github.com/windshare/windshare/core/catalog"
	"github.com/windshare/windshare/core/content"
)

const posixMutationTokenBytes = 56

func platformCatalogBaseline(file *os.File) (catalog.SourceIdentity, catalog.VersionCandidate, error) {
	return POSIXCatalogBaseline(file)
}

func newPlatformRootedRevisionSource(paths []string) (*RootedRevisionSource, error) {
	return NewRootedRevisionSource(RootedRevisionSourceConfig{RootPaths: paths, Binder: POSIXStabilityBinder{}})
}

type posixMutationToken struct {
	device      uint64
	inode       uint64
	size        int64
	modifiedSec int64
	modifiedNS  int64
	changedSec  int64
	changedNS   int64
}

func (t posixMutationToken) sourceIdentityBytes() []byte {
	result := make([]byte, 16)
	binary.BigEndian.PutUint64(result[0:8], t.device)
	binary.BigEndian.PutUint64(result[8:16], t.inode)
	return result
}

func (t posixMutationToken) candidateBytes() []byte {
	result := make([]byte, posixMutationTokenBytes)
	values := [...]uint64{
		t.device, t.inode, uint64(t.size), uint64(t.modifiedSec), uint64(t.modifiedNS), uint64(t.changedSec), uint64(t.changedNS),
	}
	for index, value := range values {
		binary.BigEndian.PutUint64(result[index*8:(index+1)*8], value)
	}
	return result
}

// POSIXCatalogBaseline captures the sender-private identity and version
// candidate that a later POSIXStabilityBinder must reproduce from the same FD.
func POSIXCatalogBaseline(file *os.File) (catalog.SourceIdentity, catalog.VersionCandidate, error) {
	token, err := platformMutationToken(file)
	if err != nil {
		return catalog.SourceIdentity{}, catalog.VersionCandidate{}, err
	}
	identity, err := catalog.NewSourceIdentity(token.sourceIdentityBytes())
	if err != nil {
		return catalog.SourceIdentity{}, catalog.VersionCandidate{}, err
	}
	candidate, err := catalog.NewVersionCandidate(token.candidateBytes())
	if err != nil {
		return catalog.SourceIdentity{}, catalog.VersionCandidate{}, err
	}
	return identity, candidate, nil
}

type POSIXStabilityBinder struct{}

func (POSIXStabilityBinder) BindStable(ctx context.Context, binding StableBinding) (content.StableFile, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if binding.File == nil {
		return nil, content.ErrUnsupportedStability
	}
	token, err := platformMutationToken(binding.File)
	if err != nil {
		return nil, err
	}
	if token.size < 0 || uint64(token.size) > catalog.MaxFileSize ||
		subtle.ConstantTimeCompare(binding.Record.SourceIdentity().Bytes(), token.sourceIdentityBytes()) != 1 ||
		subtle.ConstantTimeCompare(binding.Record.VersionCandidate().Bytes(), token.candidateBytes()) != 1 {
		return nil, content.ErrRevisionStale
	}
	modified, err := catalog.NewModifiedTime(token.modifiedSec, uint32(token.modifiedNS), catalog.TimePrecisionNanoseconds)
	if err != nil {
		return nil, fmt.Errorf("represent POSIX modified time: %w", err)
	}
	return &posixStableFile{file: binding.File, baseline: token, modified: modified}, nil
}

type posixStableFile struct {
	file     *os.File
	baseline posixMutationToken
	modified catalog.ModifiedTime
}

func (f *posixStableFile) ExactSize() uint64                  { return uint64(f.baseline.size) }
func (f *posixStableFile) ModifiedTime() catalog.ModifiedTime { return f.modified }

func (f *posixStableFile) Verify(ctx context.Context) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	current, err := platformMutationToken(f.file)
	if err != nil {
		return fmt.Errorf("inspect POSIX mutation token: %w", err)
	}
	if current != f.baseline {
		return content.ErrSourceDrift
	}
	return nil
}

func (f *posixStableFile) ReadAt(ctx context.Context, destination []byte, offset uint64) (int, error) {
	if err := ctx.Err(); err != nil {
		return 0, err
	}
	if offset > math.MaxInt64 || uint64(len(destination)) > math.MaxInt64-offset {
		return 0, content.ErrBlockOutOfRange
	}
	count, err := f.file.ReadAt(destination, int64(offset))
	if errors.Is(err, io.EOF) && count == len(destination) {
		return count, nil
	}
	return count, err
}

func (f *posixStableFile) Close() error { return f.file.Close() }
