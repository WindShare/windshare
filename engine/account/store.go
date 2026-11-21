package account

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"reflect"
	"sync"

	"github.com/windshare/windshare/engine/internal/statefile"
)

var (
	// ErrInvalidState reports a corrupt, oversized, or mismatched local account
	// file that requires manual repair instead of retrying automatically.
	ErrInvalidState = errors.New("invalid account state")
	// ErrUnreadableState reports a local account file that cannot be read due
	// to filesystem permissions or an unexpected file type.
	ErrUnreadableState = errors.New("account state unreadable")
)

func isLocalStoreError(err error) bool {
	return errors.Is(err, ErrInvalidState) || errors.Is(err, ErrUnreadableState)
}

type FileStore struct{ Path string }

// Load reads the latest committed record without taking the write lock.
// statefile.Write replaces files atomically, so concurrent readers always see a
// complete state file and never contend on an exclusive lock during polling.
func (store FileStore) Load(ctx context.Context) (Record, error) {
	if err := ctx.Err(); err != nil {
		return Record{}, err
	}
	if store.Path == "" {
		return Record{}, fmt.Errorf("%w: account storage path is empty", ErrInvalidState)
	}
	data, err := readRecord(store.Path)
	if errors.Is(err, os.ErrNotExist) {
		return Record{}, nil
	}
	if err != nil {
		return Record{}, err
	}
	var record Record
	if json.Unmarshal(data, &record) != nil {
		return Record{}, ErrInvalidState
	}
	return record, nil
}

func (store FileStore) Update(ctx context.Context, apply func(Record) (Record, error)) (Record, error) {
	var result Record
	if store.Path == "" {
		return result, fmt.Errorf("%w: account storage path is empty", ErrInvalidState)
	}
	err := statefile.WithLock(ctx, store.Path, func() error {
		var previous Record
		data, err := readRecord(store.Path)
		if err != nil && !errors.Is(err, os.ErrNotExist) {
			return err
		}
		if err == nil && json.Unmarshal(data, &previous) != nil {
			return ErrInvalidState
		}
		result, err = apply(cloneRecord(previous))
		if err != nil {
			return err
		}
		if reflect.DeepEqual(previous, result) {
			return nil
		}
		data, err = json.Marshal(result)
		if err != nil {
			return err
		}
		return statefile.Write(ctx, store.Path, data)
	})
	if errors.Is(err, os.ErrPermission) && !isLocalStoreError(err) {
		return result, fmt.Errorf("%w: %w", ErrUnreadableState, err)
	}
	return result, err
}

func readRecord(path string) ([]byte, error) {
	file, err := os.Open(path)
	if err != nil {
		if errors.Is(err, os.ErrPermission) {
			return nil, fmt.Errorf("%w: %w", ErrUnreadableState, err)
		}
		return nil, err
	}
	defer file.Close()
	info, err := file.Stat()
	if err != nil {
		return nil, err
	}
	if !info.Mode().IsRegular() {
		return nil, fmt.Errorf("%w: account state must be a regular file", ErrUnreadableState)
	}
	const maximumRecordBytes = 16 << 10
	data, err := io.ReadAll(io.LimitReader(file, maximumRecordBytes+1))
	if err != nil {
		if errors.Is(err, os.ErrPermission) {
			return nil, fmt.Errorf("%w: %w", ErrUnreadableState, err)
		}
		return nil, err
	}
	if len(data) > maximumRecordBytes {
		return nil, fmt.Errorf("%w: account state too large", ErrInvalidState)
	}
	return data, nil
}

type MemoryStore struct {
	mu     sync.Mutex
	record Record
}

func (store *MemoryStore) Load(ctx context.Context) (Record, error) {
	if err := ctx.Err(); err != nil {
		return Record{}, err
	}
	store.mu.Lock()
	defer store.mu.Unlock()
	return cloneRecord(store.record), nil
}

func (store *MemoryStore) Update(ctx context.Context, apply func(Record) (Record, error)) (Record, error) {
	if err := ctx.Err(); err != nil {
		return Record{}, err
	}
	store.mu.Lock()
	defer store.mu.Unlock()
	next, err := apply(cloneRecord(store.record))
	if err != nil {
		return Record{}, err
	}
	store.record = cloneRecord(next)
	return cloneRecord(next), nil
}
func cloneRecord(value Record) Record {
	if value.Credential != nil {
		credential := *value.Credential
		value.Credential = &credential
	}
	if value.Proof != nil {
		proof := *value.Proof
		value.Proof = &proof
	}
	return value
}
