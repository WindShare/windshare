package statefile

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"sort"
)

const (
	Settings           = "settings"
	maximumCommitBytes = 256 * 1024
	commitName         = "commit.json"
)

type mutation struct {
	Name   string `json:"name"`
	Data   []byte `json:"data,omitempty"`
	Delete bool   `json:"delete,omitempty"`
}

// Transaction coordinates settings and operation records in one local namespace.
// Optional discovery caches do not participate in these commits.
type Transaction struct {
	path    string
	changes map[string]mutation
}

func StateDirectory(path string) string { return path + ".state" }

// ReadSettings needs no writable directory in the ordinary sharing path. A
// pending multi-file commit is recovered first; otherwise atomic replacement
// guarantees that a concurrent writer exposes either complete settings version.
func ReadSettings(ctx context.Context, path string) ([]byte, error) {
	if _, err := os.Stat(filepath.Join(StateDirectory(path), commitName)); errors.Is(err, os.ErrNotExist) {
		return (&Transaction{path: path}).Read(Settings)
	}
	var result []byte
	err := Transact(ctx, path, func(tx *Transaction) error {
		var err error
		result, err = tx.Read(Settings)
		return err
	})
	return result, err
}

func Transact(ctx context.Context, path string, action func(*Transaction) error) error {
	return WithLock(ctx, path, func() error {
		tx := &Transaction{path: path, changes: make(map[string]mutation)}
		if err := tx.recover(); err != nil {
			return err
		}
		if err := action(tx); err != nil {
			return err
		}
		return tx.commit()
	})
}

func (tx *Transaction) location(name string) (string, error) {
	if name == Settings {
		return tx.path, nil
	}
	// Records cannot escape the settings namespace or replace the recovery log.
	if !filepath.IsLocal(name) || filepath.Dir(filepath.FromSlash(name)) != "setup" || filepath.Ext(name) != ".json" {
		return "", errors.New("invalid operation record name")
	}
	return filepath.Join(StateDirectory(tx.path), filepath.FromSlash(name)), nil
}

func (tx *Transaction) Read(name string) ([]byte, error) {
	if edit, ok := tx.changes[name]; ok {
		if edit.Delete {
			return nil, os.ErrNotExist
		}
		return bytes.Clone(edit.Data), nil
	}
	path, err := tx.location(name)
	if err != nil {
		return nil, err
	}
	file, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer file.Close()
	data, err := io.ReadAll(io.LimitReader(file, maximumCommitBytes+1))
	if err != nil {
		return nil, err
	}
	if len(data) > maximumCommitBytes {
		return nil, errors.New("state record too large")
	}
	return data, nil
}

func (tx *Transaction) Operations() ([]string, error) {
	entries, err := os.ReadDir(filepath.Join(StateDirectory(tx.path), "setup"))
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var names []string
	for _, entry := range entries {
		if !entry.IsDir() && filepath.Ext(entry.Name()) == ".json" {
			names = append(names, "setup/"+entry.Name())
		}
	}
	return names, nil
}

func (tx *Transaction) Put(name string, data []byte) error {
	if _, err := tx.location(name); err != nil {
		return err
	}
	tx.changes[name] = mutation{Name: name, Data: bytes.Clone(data)}
	return nil
}

func (tx *Transaction) Delete(name string) error {
	if name == Settings {
		return errors.New("settings cannot be deleted by an operation")
	}
	if _, err := tx.location(name); err != nil {
		return err
	}
	tx.changes[name] = mutation{Name: name, Delete: true}
	return nil
}

func (tx *Transaction) apply(changes []mutation) error {
	for _, edit := range changes {
		path, err := tx.location(edit.Name)
		if err != nil {
			return err
		}
		if edit.Delete {
			err = Remove(path)
		} else {
			err = Write(path, edit.Data)
		}
		if err != nil {
			return err
		}
	}
	return nil
}

func (tx *Transaction) commit() error {
	if len(tx.changes) == 0 {
		return nil
	}
	changes := make([]mutation, 0, len(tx.changes))
	for _, change := range tx.changes {
		changes = append(changes, change)
	}
	sort.Slice(changes, func(i, j int) bool { return changes[i].Name < changes[j].Name })
	if len(changes) == 1 {
		return tx.apply(changes)
	}
	data, err := json.Marshal(changes)
	if err != nil {
		return err
	}
	if len(data) > maximumCommitBytes {
		return errors.New("local commit is too large")
	}
	// Publishing this intent commits the whole operation. Every subsequent reader
	// or importer finishes it before seeing or replacing the settings.
	if err := Write(filepath.Join(StateDirectory(tx.path), commitName), data); err != nil {
		return err
	}
	slog.Debug("local_state_commit", "settings_path", tx.path, "records", recordNames(changes))
	return tx.recover()
}

func (tx *Transaction) recover() error {
	path := filepath.Join(StateDirectory(tx.path), commitName)
	file, err := os.Open(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	data, err := io.ReadAll(io.LimitReader(file, maximumCommitBytes+1))
	_ = file.Close()
	if err != nil {
		return err
	}
	if len(data) > maximumCommitBytes {
		return errors.New("invalid local commit size")
	}
	var changes []mutation
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&changes); err != nil {
		return err
	}
	var trailing any
	if decoder.Decode(&trailing) != io.EOF || len(changes) < 2 {
		return errors.New("invalid local commit")
	}
	seen := make(map[string]bool)
	for _, change := range changes {
		if _, err := tx.location(change.Name); err != nil {
			return err
		}
		if seen[change.Name] || change.Delete && (change.Name == Settings || len(change.Data) != 0) {
			return errors.New("invalid local commit mutation")
		}
		seen[change.Name] = true
	}
	if err := tx.apply(changes); err != nil {
		return fmt.Errorf("local commit awaiting recovery: %w", err)
	}
	if err := Remove(path); err != nil {
		return fmt.Errorf("local commit awaiting cleanup: %w", err)
	}
	slog.Debug("local_state_committed", "settings_path", tx.path, "records", recordNames(changes))
	return nil
}

func recordNames(changes []mutation) []string {
	names := make([]string, 0, len(changes))
	for _, change := range changes {
		names = append(names, change.Name)
	}
	return names
}
