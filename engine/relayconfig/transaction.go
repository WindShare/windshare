package relayconfig

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"os"

	"github.com/windshare/windshare/engine/internal/statefile"
)

func (config Config) SettingsRevision() string {
	// Revision equality must cover every persisted setting. Runtime relay values
	// omit access keys from JSON, which would hide credential-only replacements.
	data, _ := json.Marshal(config.document())
	digest := sha256.Sum256(data)
	return base64.RawURLEncoding.EncodeToString(digest[:])
}

// Update serializes user edits across processes. Network activity never runs in
// its callback; setup commits use the same storage transaction boundary.
func (store FileStore) Update(ctx context.Context, change func(*Config) error) (Config, error) {
	var committed Config
	path, err := store.Location()
	if err != nil {
		return committed, err
	}
	err = statefile.Transact(ctx, path, func(tx *statefile.Transaction) error {
		current, err := ReadSettings(tx)
		if err != nil {
			return err
		}
		if err := change(&current); err != nil {
			return err
		}
		data, err := encode(current)
		if err != nil {
			return err
		}
		if err := tx.Put(statefile.Settings, data); err != nil {
			return err
		}
		committed = current
		return nil
	})
	return committed, err
}

func (store FileStore) save(config Config) error {
	path, err := store.Location()
	if err != nil {
		return err
	}
	data, err := encode(config)
	if err != nil {
		return err
	}
	return statefile.Transact(context.Background(), path, func(tx *statefile.Transaction) error {
		return tx.Put(statefile.Settings, data)
	})
}

// ReadSettings lets the setup store observe settings under the shared commit lock.
func ReadSettings(tx *statefile.Transaction) (Config, error) {
	data, err := tx.Read(statefile.Settings)
	if errors.Is(err, os.ErrNotExist) {
		return Config{}, nil
	}
	if err != nil {
		return Config{}, err
	}
	return Decode(bytes.NewReader(data))
}
