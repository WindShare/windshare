// Package namestore persists receiver-local name choices under the operation lease.
package namestore

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io/fs"
	"sort"

	"github.com/windshare/windshare/osfs/internal/checkpointstore"
	"github.com/windshare/windshare/osfs/internal/outputnames"
)

const format = "windshare/native-name-mapping/v1"

type diskRecord struct {
	Format      string `json:"format"`
	OperationID string `json:"operationId"`
	outputnames.Mapping
}

type Store struct {
	lease       *checkpointstore.OperationRegistryLease
	operationID string
	fresh       bool
	records     map[string]outputnames.Mapping
}

func New(lease *checkpointstore.OperationRegistryLease, operation []byte, fresh bool) *Store {
	return &Store{lease: lease, operationID: hex.EncodeToString(operation), fresh: fresh, records: make(map[string]outputnames.Mapping)}
}

func (store *Store) Load() (result []outputnames.Mapping, resultErr error) {
	if store.lease == nil || store.fresh {
		return store.snapshot(), nil
	}
	directory, err := store.lease.OpenNameState(false)
	if errors.Is(err, fs.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	defer func() { resultErr = errors.Join(resultErr, directory.Close()) }()
	if err := store.lease.ReclaimNameCandidates(directory); err != nil {
		return nil, err
	}
	names, err := directory.Names(outputnames.MaximumMappings)
	if err != nil {
		return nil, err
	}
	for _, name := range names {
		encoded, err := checkpointstore.ReadFile(directory, name)
		if err != nil {
			return nil, err
		}
		var record diskRecord
		if err := json.Unmarshal(encoded, &record); err != nil ||
			record.Format != format || record.OperationID != store.operationID ||
			mappingName(record.LogicalPath) != name {
			return nil, outputnames.ErrInvalidMapping
		}
		canonical, err := store.encode(record.Mapping)
		if err != nil || !bytes.Equal(encoded, canonical) {
			return nil, outputnames.ErrInvalidMapping
		}
		store.records[record.LogicalPath] = record.Mapping
	}
	store.fresh = true
	return store.snapshot(), nil
}

func (store *Store) Put(mapping outputnames.Mapping) error {
	if previous, exists := store.records[mapping.LogicalPath]; exists {
		if previous != mapping {
			return outputnames.ErrInvalidMapping
		}
		return nil
	}
	encoded, err := store.encode(mapping)
	if err != nil {
		return err
	}
	if store.lease != nil {
		if err := store.lease.InstallNameMapping(mappingName(mapping.LogicalPath), encoded); err != nil {
			return err
		}
	}
	store.records[mapping.LogicalPath] = mapping
	return nil
}

func (store *Store) Clear() (resultErr error) {
	if store.lease == nil || len(store.records) == 0 {
		return nil
	}
	directory, err := store.lease.OpenNameState(false)
	if errors.Is(err, fs.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	defer func() { resultErr = errors.Join(resultErr, directory.Close()) }()
	if err := store.lease.ReclaimNameCandidates(directory); err != nil {
		return err
	}
	// Retire descendants first. If cleanup stops halfway, every surviving child
	// still has the ancestor mappings needed to describe its real saved path.
	records := store.snapshot()
	sort.Slice(records, func(i, j int) bool { return records[i].LogicalPath > records[j].LogicalPath })
	for _, record := range records {
		encoded, err := store.encode(record)
		if err != nil {
			return err
		}
		removeErr, closeErr := checkpointstore.RemoveExact(directory, mappingName(record.LogicalPath), encoded)
		if closeErr != nil || removeErr != nil && !errors.Is(removeErr, fs.ErrNotExist) {
			return errors.Join(removeErr, closeErr)
		}
	}
	return nil
}

func (store *Store) snapshot() []outputnames.Mapping {
	records := make([]outputnames.Mapping, 0, len(store.records))
	for _, record := range store.records {
		records = append(records, record)
	}
	return records
}

func (store *Store) encode(mapping outputnames.Mapping) ([]byte, error) {
	return json.Marshal(diskRecord{Format: format, OperationID: store.operationID, Mapping: mapping})
}

func mappingName(logical string) string {
	digest := sha256.Sum256([]byte(logical))
	return hex.EncodeToString(digest[:]) + ".json"
}
