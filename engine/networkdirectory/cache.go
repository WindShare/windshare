package networkdirectory

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"maps"
	"os"
	"path/filepath"
	"sync"

	"github.com/windshare/windshare/core/nodedirectory"
	"github.com/windshare/windshare/engine/internal/statefile"
)

const maximumCacheBytes = 2 * nodedirectory.MaximumEnvelopeBytes

type Record struct {
	Cursor   nodedirectory.Cursor `json:"cursor"`
	Envelope []byte               `json:"envelope"`
	Aliases  map[string]string    `json:"aliases"`
}

func (record Record) clone() Record {
	record.Envelope = append([]byte(nil), record.Envelope...)
	record.Aliases = maps.Clone(record.Aliases)
	return record
}

// Cache commits the signed snapshot and rollback cursor together. Update's
// callback must run under the same lock as the read, including across processes.
type Cache interface {
	Load(context.Context) (Record, error)
	Update(context.Context, func(Record) (Record, error)) (Record, error)
}

type MemoryCache struct {
	mu     sync.Mutex
	record Record
}

func (cache *MemoryCache) Load(context.Context) (Record, error) {
	cache.mu.Lock()
	defer cache.mu.Unlock()
	return cache.record.clone(), nil
}
func (cache *MemoryCache) Update(_ context.Context, change func(Record) (Record, error)) (Record, error) {
	cache.mu.Lock()
	defer cache.mu.Unlock()
	next, err := change(cache.record.clone())
	if err == nil {
		cache.record = next.clone()
	}
	return next, err
}

type FileCache struct{ path string }

func NewFileCache(configPath string) *FileCache {
	return &FileCache{path: filepath.Join(statefile.StateDirectory(configPath), "network", "directory.json")}
}

func (cache *FileCache) Load(context.Context) (Record, error) { return cache.read() }
func (cache *FileCache) read() (Record, error) {
	file, err := os.Open(cache.path)
	if errors.Is(err, os.ErrNotExist) {
		return Record{}, nil
	}
	if err != nil {
		return Record{}, err
	}
	defer file.Close()
	data, err := io.ReadAll(io.LimitReader(file, maximumCacheBytes+1))
	if err != nil {
		return Record{}, err
	}
	if len(data) > maximumCacheBytes {
		return Record{}, nodedirectory.ErrFormat
	}
	var record Record
	if err = json.Unmarshal(data, &record); err != nil {
		return Record{}, err
	}
	return record, nil
}
func (cache *FileCache) Update(ctx context.Context, change func(Record) (Record, error)) (Record, error) {
	var committed Record
	err := statefile.WithLock(ctx, cache.path, func() error {
		record, err := cache.read()
		if err != nil {
			return err
		}
		committed, err = change(record)
		if err != nil {
			return err
		}
		data, err := json.Marshal(committed)
		if err != nil {
			return err
		}
		if len(data) > maximumCacheBytes {
			return nodedirectory.ErrFormat
		}
		return statefile.Write(cache.path, data)
	})
	return committed, err
}
