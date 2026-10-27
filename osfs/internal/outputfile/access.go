// Package outputfile exposes purpose-specific access to a retained native file.
// Native states own identity, permissions and rollback; these wrappers keep
// observation and durability handles from accidentally gaining write authority.
package outputfile

import (
	"errors"
	"io"

	"github.com/windshare/windshare/core/catalog"
	"github.com/windshare/windshare/osfs/internal/outputcap"
)

var ErrClosed = errors.Join(outputcap.ErrUnsafeNamespace, errors.New("output file authority is closed"))

type Identity interface {
	Close() error
	Size() (uint64, error)
	SameFile(outputcap.FileIdentity) (bool, error)
}

type Observation interface {
	Identity
	io.ReaderAt
	MetadataMatches(uint64, catalog.ModifiedTime) (bool, error)
}

type Durability interface {
	Identity
	Sync() error
}

type Mutation interface {
	Observation
	Durability
	io.WriterAt
	SetModifiedTime(catalog.ModifiedTime) error
}

type Observed[S Observation] struct{ State S }
type Recovery[S Durability] struct{ State S }
type Mutable[S Mutation] struct{ State S }

func (file *Observed[S]) Close() error {
	if file == nil {
		return nil
	}
	return file.State.Close()
}
func (file *Observed[S]) Size() (uint64, error) {
	if file == nil {
		return 0, ErrClosed
	}
	return file.State.Size()
}
func (file *Observed[S]) SameFile(other outputcap.FileIdentity) (bool, error) {
	if file == nil {
		return false, ErrClosed
	}
	return file.State.SameFile(other)
}
func (file *Observed[S]) ReadAt(data []byte, offset int64) (int, error) {
	if file == nil {
		return 0, ErrClosed
	}
	return file.State.ReadAt(data, offset)
}
func (file *Observed[S]) MetadataMatches(size uint64, modified catalog.ModifiedTime) (bool, error) {
	if file == nil {
		return false, ErrClosed
	}
	return file.State.MetadataMatches(size, modified)
}

func (file *Recovery[S]) Close() error {
	if file == nil {
		return nil
	}
	return file.State.Close()
}
func (file *Recovery[S]) Size() (uint64, error) {
	if file == nil {
		return 0, ErrClosed
	}
	return file.State.Size()
}
func (file *Recovery[S]) SameFile(other outputcap.FileIdentity) (bool, error) {
	if file == nil {
		return false, ErrClosed
	}
	return file.State.SameFile(other)
}
func (file *Recovery[S]) Sync() error {
	if file == nil {
		return ErrClosed
	}
	return file.State.Sync()
}

func (file *Mutable[S]) Close() error {
	if file == nil {
		return nil
	}
	return file.State.Close()
}
func (file *Mutable[S]) Size() (uint64, error) {
	if file == nil {
		return 0, ErrClosed
	}
	return file.State.Size()
}
func (file *Mutable[S]) SameFile(other outputcap.FileIdentity) (bool, error) {
	if file == nil {
		return false, ErrClosed
	}
	return file.State.SameFile(other)
}
func (file *Mutable[S]) ReadAt(data []byte, offset int64) (int, error) {
	if file == nil {
		return 0, ErrClosed
	}
	return file.State.ReadAt(data, offset)
}
func (file *Mutable[S]) WriteAt(data []byte, offset int64) (int, error) {
	if file == nil {
		return 0, ErrClosed
	}
	return file.State.WriteAt(data, offset)
}
func (file *Mutable[S]) Sync() error {
	if file == nil {
		return ErrClosed
	}
	return file.State.Sync()
}
func (file *Mutable[S]) SetModifiedTime(modified catalog.ModifiedTime) error {
	if file == nil {
		return ErrClosed
	}
	return file.State.SetModifiedTime(modified)
}
func (file *Mutable[S]) MetadataMatches(size uint64, modified catalog.ModifiedTime) (bool, error) {
	if file == nil {
		return false, ErrClosed
	}
	return file.State.MetadataMatches(size, modified)
}
