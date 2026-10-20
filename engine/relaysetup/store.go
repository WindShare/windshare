package relaysetup

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"

	"github.com/windshare/windshare/engine/internal/statefile"
	"github.com/windshare/windshare/engine/relayconfig"
)

// Snapshot is a setup transaction's view, never a configuration document.
type Snapshot struct {
	relayconfig.Config
	Pending *PendingSetup
}

type FileStore struct{ Settings relayconfig.FileStore }

func (store FileStore) Load() (Snapshot, error) {
	return store.Update(context.Background(), func(*Snapshot) error { return nil })
}

func loadSnapshot(tx *statefile.Transaction) (Snapshot, error) {
	settings, err := relayconfig.ReadSettings(tx)
	if err != nil {
		return Snapshot{}, err
	}
	names, err := tx.Operations()
	if err != nil {
		return Snapshot{}, err
	}
	if len(names) > 1 {
		return Snapshot{}, ErrPendingSetup
	}
	snapshot := Snapshot{Config: settings}
	if len(names) == 1 {
		data, err := tx.Read(names[0])
		if err != nil {
			return Snapshot{}, err
		}
		var pending PendingSetup
		if err := json.Unmarshal(data, &pending); err != nil {
			return Snapshot{}, err
		}
		if names[0] != recordName(&pending) {
			return Snapshot{}, errors.New("setup record identity mismatch")
		}
		snapshot.Pending = &pending
	}
	return snapshot, nil
}

func recordName(pending *PendingSetup) string { return "setup/" + pending.OperationID + ".json" }

func (store FileStore) Update(ctx context.Context, change func(*Snapshot) error) (Snapshot, error) {
	path, err := store.Settings.Location()
	if err != nil {
		return Snapshot{}, err
	}
	var result Snapshot
	err = statefile.Transact(ctx, path, func(tx *statefile.Transaction) error {
		current, err := loadSnapshot(tx)
		if err != nil {
			return err
		}
		revision, before := current.SettingsRevision(), current.Pending
		if before != nil {
			copy := *before
			current.Pending = &copy
		}
		if err := change(&current); err != nil {
			return err
		}
		if err := stageSettings(tx, revision, current.Config); err != nil {
			return err
		}
		if err := stageCheckpoint(tx, before, current.Pending); err != nil {
			return err
		}
		result = current
		return nil
	})
	return result, err
}

func stageSettings(tx *statefile.Transaction, revision string, next relayconfig.Config) error {
	if revision == next.SettingsRevision() {
		return nil
	}
	data, err := relayconfig.Encode(next)
	if err != nil {
		return err
	}
	return tx.Put(statefile.Settings, data)
}

func stageCheckpoint(tx *statefile.Transaction, before, next *PendingSetup) error {
	if next == nil {
		if before == nil {
			return nil
		}
		return tx.Delete(recordName(before))
	}
	data, err := json.Marshal(next)
	if err != nil {
		return err
	}
	previous, err := json.Marshal(before)
	if err != nil {
		return err
	}
	if !bytes.Equal(data, previous) {
		if err := tx.Put(recordName(next), data); err != nil {
			return err
		}
	}
	if before != nil && next.OperationID != before.OperationID {
		return tx.Delete(recordName(before))
	}
	return nil
}
