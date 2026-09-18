package osfs

import (
	"encoding/binary"
	"errors"
	"fmt"
	"strings"
	"unicode/utf8"

	"github.com/windshare/windshare/core/catalog"
)

// RootSlot indexes the retained root authorities owned by one filesystem source.
type RootSlot uint16

const sourceReferenceHeaderBytes = 3
const sourceReferenceVersion = 1

type sourceLocation struct {
	slot RootSlot
	path string
}

func (location sourceLocation) RootSlot() RootSlot   { return location.slot }
func (location sourceLocation) RelativePath() string { return location.path }

// NewSourceReference preserves native spelling; canonical public names must not
// change which inode the private reference opens on byte-sensitive filesystems.
func NewSourceReference(slot RootSlot, relativePath string) (catalog.SourceReference, error) {
	if uint64(slot) >= catalog.MaxSelectedRoots {
		return catalog.SourceReference{}, errors.New("osfs: source root slot exceeds selected-root limit")
	}
	if err := validateSourcePath(relativePath); err != nil {
		return catalog.SourceReference{}, err
	}
	raw := make([]byte, sourceReferenceHeaderBytes+len(relativePath))
	raw[0] = sourceReferenceVersion
	binary.BigEndian.PutUint16(raw[1:sourceReferenceHeaderBytes], uint16(slot))
	copy(raw[sourceReferenceHeaderBytes:], relativePath)
	return catalog.NewSourceReference(raw)
}

func parseSourceReference(reference catalog.SourceReference) (sourceLocation, error) {
	raw := reference.Bytes()
	if len(raw) < sourceReferenceHeaderBytes || raw[0] != sourceReferenceVersion {
		return sourceLocation{}, errors.New("osfs: invalid filesystem source reference")
	}
	slot := RootSlot(binary.BigEndian.Uint16(raw[1:sourceReferenceHeaderBytes]))
	path := string(raw[sourceReferenceHeaderBytes:])
	if uint64(slot) >= catalog.MaxSelectedRoots {
		return sourceLocation{}, errors.New("osfs: source root slot exceeds selected-root limit")
	}
	if err := validateSourcePath(path); err != nil {
		return sourceLocation{}, err
	}
	return sourceLocation{slot: slot, path: path}, nil
}

func validateSourcePath(path string) error {
	if path == "" {
		return nil
	}
	if !utf8.ValidString(path) || len(path) > catalog.MaxPathBytes || strings.ContainsRune(path, '\\') ||
		strings.HasPrefix(path, "/") || strings.HasSuffix(path, "/") {
		return fmt.Errorf("%w: source path must be relative, valid UTF-8, and slash-separated", catalog.ErrInvalidPath)
	}
	components := strings.Split(path, "/")
	if len(components) > catalog.MaxPathDepth {
		return fmt.Errorf("%w: got %d components", catalog.ErrPathTooDeep, len(components))
	}
	for index, component := range components {
		if len(component) > catalog.MaxNameBytes {
			return fmt.Errorf("%w: component %d is too long", catalog.ErrInvalidPath, index)
		}
		if _, err := catalog.CanonicalName(component); err != nil {
			return fmt.Errorf("%w: component %d: %w", catalog.ErrInvalidPath, index, err)
		}
	}
	return nil
}
