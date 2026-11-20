package account

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"os"
	"reflect"
	"sync"

	"github.com/windshare/windshare/engine/internal/statefile"
)

type FileStore struct{ Path string }

// Load reads the latest committed record without taking the write lock.
// statefile.Write replaces files atomically, so concurrent readers always see a
// complete state file and never contend on an exclusive lock during polling.
func (store FileStore) Load(ctx context.Context) (Record, error) {
	if err := ctx.Err(); err != nil {
		return Record{}, err
	}
	if store.Path == "" {
		return Record{}, errors.New("account storage path is empty")
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
		return Record{}, errors.New("invalid account state")
	}
	return record, nil
}

func (store FileStore) Update(ctx context.Context, apply func(Record) (Record, error)) (Record, error) {
	var result Record
	if store.Path == "" {
		return result, errors.New("account storage path is empty")
	}
	err := statefile.WithLock(ctx, store.Path, func() error {
		var previous Record
		data, err := readRecord(store.Path)
		if err != nil && !errors.Is(err, os.ErrNotExist) {
			return err
		}
		if err == nil && json.Unmarshal(data, &previous) != nil {
			return errors.New("invalid account state")
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
		return statefile.Write(store.Path, data)
	})
	return result, err
}

func readRecord(path string) ([]byte, error) {
	file, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer file.Close()
	const maximumRecordBytes = 16 << 10
	data, err := io.ReadAll(io.LimitReader(file, maximumRecordBytes+1))
	if len(data) > maximumRecordBytes {
		return nil, errors.New("account state too large")
	}
	return data, err
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
