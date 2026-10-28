// Package deliveryreceipts keeps process-local delivery history with bounded
// memory. Anonymous sorted runs extend capacity without restart authority.
package deliveryreceipts

import (
	"bytes"
	"crypto/sha256"
	"errors"
	"io"
	"slices"

	"github.com/windshare/windshare/core/transfer"
	"github.com/windshare/windshare/osfs/internal/fileexecution"
)

const maximumMemoryReceipts = 1024

var (
	errClosed   = errors.New("live delivery receipts are closed")
	errConflict = errors.New("live delivery receipt conflicts with earlier delivery")
	errInvalid  = errors.New("invalid live delivery receipt")
)

type scratchFile interface {
	io.ReaderAt
	io.Writer
	io.Closer
}

type entry struct {
	key     transfer.MaterializationLocatorDigest
	receipt fileexecution.DeliveryReceipt
}

// Store is serialized by its operation owner. Small operations need no scratch
// storage; larger ones retain one immutable run per occupied binary level.
type Store struct {
	memory   map[transfer.MaterializationLocatorDigest]fileexecution.DeliveryReceipt
	runs     []*run
	newFile  func() (scratchFile, error)
	closeErr error
	closed   bool
}

func New() *Store {
	return &Store{newFile: newScratchFile}
}

func (store *Store) Lookup(key transfer.MaterializationLocatorDigest) (fileexecution.DeliveryReceipt, bool, error) {
	if store.closed {
		return fileexecution.DeliveryReceipt{}, false, errClosed
	}
	if receipt, ok := store.memory[key]; ok {
		return receipt, true, nil
	}
	for _, current := range store.runs {
		if current == nil {
			continue
		}
		receipt, found, err := current.lookup(key)
		if err != nil || found {
			return receipt, found, err
		}
	}
	return fileexecution.DeliveryReceipt{}, false, nil
}

func (store *Store) Record(key transfer.MaterializationLocatorDigest, receipt fileexecution.DeliveryReceipt) error {
	if key == (transfer.MaterializationLocatorDigest{}) || receipt.DescriptorDigest == ([sha256.Size]byte{}) || receipt.Object.IsZero() {
		return errInvalid
	}
	previous, found, err := store.Lookup(key)
	if err != nil {
		return err
	}
	if found {
		if previous != receipt {
			return errConflict
		}
		return nil
	}
	// Flush before admitting another receipt: an I/O failure leaves both the
	// memory budget and every previously acknowledged delivery intact.
	if len(store.memory) == maximumMemoryReceipts {
		if err := store.flush(); err != nil {
			return err
		}
	}
	if store.memory == nil {
		store.memory = make(map[transfer.MaterializationLocatorDigest]fileexecution.DeliveryReceipt)
	}
	store.memory[key] = receipt
	return nil
}

func (store *Store) flush() error {
	entries := make([]entry, 0, len(store.memory))
	for key, receipt := range store.memory {
		entries = append(entries, entry{key: key, receipt: receipt})
	}
	slices.SortFunc(entries, func(a, b entry) int { return bytes.Compare(a.key[:], b.key[:]) })
	candidate, err := writeRun(store.newFile, entries)
	if err != nil {
		return err
	}
	level := 0
	for level < len(store.runs) && store.runs[level] != nil {
		merged, mergeErr := mergeRuns(store.newFile, candidate, store.runs[level])
		if mergeErr != nil {
			return errors.Join(mergeErr, candidate.file.Close())
		}
		store.rememberCloseError(candidate.file.Close())
		candidate = merged
		level++
	}
	// Installation happens only after all merges succeed. Old runs remain
	// readable across allocation, short-write, and merge-read failures.
	for index := 0; index < level; index++ {
		store.rememberCloseError(store.runs[index].file.Close())
		store.runs[index] = nil
	}
	if level == len(store.runs) {
		store.runs = append(store.runs, candidate)
	} else {
		store.runs[level] = candidate
	}
	clear(store.memory)
	return nil
}

func (store *Store) rememberCloseError(err error) {
	if store.closeErr == nil {
		store.closeErr = err
	}
}

func (store *Store) Close() error {
	if store.closed {
		return nil
	}
	store.closed = true
	for _, current := range store.runs {
		if current != nil {
			store.rememberCloseError(current.file.Close())
		}
	}
	store.memory, store.runs = nil, nil
	return store.closeErr
}
