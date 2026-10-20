package statefile

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"testing"
)

func TestRecoveryCompletesSettingsAndOperationRemovalBeforeImport(t *testing.T) {
	for _, applied := range []int{0, 1, 2} {
		t.Run(string(rune('0'+applied)), func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "config.json")
			record := "setup/operation.json"
			tx := &Transaction{path: path}
			if err := Write(path, []byte("before")); err != nil {
				t.Fatal(err)
			}
			journal, _ := tx.location(record)
			if err := Write(journal, []byte("installing")); err != nil {
				t.Fatal(err)
			}
			changes := []mutation{{Name: Settings, Data: []byte("installed")}, {Name: record, Delete: true}}
			data, _ := json.Marshal(changes)
			if err := Write(filepath.Join(StateDirectory(path), commitName), data); err != nil {
				t.Fatal(err)
			}
			if err := tx.apply(changes[:applied]); err != nil {
				t.Fatal(err)
			}
			// Each prefix models a process exit after a durable commit step.
			recovered, err := ReadSettings(t.Context(), path)
			if err != nil || string(recovered) != "installed" {
				t.Fatal("settings load did not recover", err)
			}
			err = Transact(t.Context(), path, func(current *Transaction) error {
				data, err := current.Read(Settings)
				if err != nil || string(data) != "installed" {
					t.Fatal("reader observed partial installation", string(data), err)
				}
				names, err := current.Operations()
				if err != nil || len(names) != 0 {
					t.Fatal("installed credential could still be cancelled", names, err)
				}
				return current.Put(Settings, []byte("later import"))
			})
			if err != nil {
				t.Fatal(err)
			}
			if err := Transact(t.Context(), path, func(current *Transaction) error {
				data, err := current.Read(Settings)
				if err != nil || string(data) != "later import" {
					t.Fatal("recovery overwrote newer settings", string(data), err)
				}
				return nil
			}); err != nil {
				t.Fatal(err)
			}
			if _, err := os.Stat(filepath.Join(StateDirectory(path), commitName)); !errors.Is(err, os.ErrNotExist) {
				t.Fatal("commit was not cleaned", err)
			}
		})
	}
}

func TestTransactionCommitsCheckpointAndInstallation(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.json")
	if err := Transact(t.Context(), path, func(tx *Transaction) error {
		if err := tx.Put("setup/operation.json", []byte("approval")); err != nil {
			return err
		}
		data, err := tx.Read("setup/operation.json")
		if err != nil || string(data) != "approval" {
			t.Fatal(err)
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if err := Transact(t.Context(), path, func(tx *Transaction) error {
		if err := tx.Put(Settings, []byte("installed")); err != nil {
			return err
		}
		if err := tx.Delete("setup/operation.json"); err != nil {
			return err
		}
		if _, err := tx.Read("setup/operation.json"); !errors.Is(err, os.ErrNotExist) {
			t.Fatal(err)
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
}

func TestRejectedTransactionAndCancelledLockDoNotPublish(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.json")
	original := []byte("original")
	if err := Write(path, original); err != nil {
		t.Fatal(err)
	}
	failure := errors.New("rejected")
	err := Transact(t.Context(), path, func(tx *Transaction) error {
		if err := tx.Put(Settings, []byte("uncommitted")); err != nil {
			return err
		}
		return failure
	})
	if !errors.Is(err, failure) {
		t.Fatal(err)
	}
	after, err := os.ReadFile(path)
	if err != nil || string(after) != string(original) {
		t.Fatal("callback failure changed settings", err)
	}
	if err := WithLock(t.Context(), path, func() error {
		ctx, cancel := context.WithCancel(t.Context())
		cancel()
		return Transact(ctx, path, func(*Transaction) error { t.Fatal("cancelled transaction ran"); return nil })
	}); !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
}

func TestInvalidRecoveryLogCannotEscapeOrPartiallyApply(t *testing.T) {
	for _, changes := range [][]mutation{
		{{Name: Settings, Data: []byte("bad")}, {Name: "../outside", Delete: true}},
		{{Name: Settings, Data: []byte("bad")}, {Name: Settings, Data: []byte("duplicate")}},
		{{Name: Settings, Delete: true}, {Name: "setup/a.json", Delete: true}},
	} {
		path := filepath.Join(t.TempDir(), "config.json")
		if err := Write(path, []byte("original")); err != nil {
			t.Fatal(err)
		}
		data, _ := json.Marshal(changes)
		if err := Write(filepath.Join(StateDirectory(path), commitName), data); err != nil {
			t.Fatal(err)
		}
		if err := Transact(t.Context(), path, func(*Transaction) error { t.Fatal("invalid commit admitted reader"); return nil }); err == nil {
			t.Fatal("invalid recovery log accepted")
		}
		data, _ = os.ReadFile(path)
		if string(data) != "original" {
			t.Fatal("partially applied an invalid commit")
		}
	}
}

func TestFailedPublicationKeepsRecoveryIntent(t *testing.T) {
	directory := t.TempDir()
	path := filepath.Join(directory, "config.json")
	// A directory at the destination causes publication to fail before replacing it.
	if err := os.Mkdir(path, privateDirectoryMode); err != nil {
		t.Fatal(err)
	}
	err := Transact(t.Context(), path, func(tx *Transaction) error {
		if err := tx.Put(Settings, []byte("installed")); err != nil {
			return err
		}
		return tx.Put("setup/operation.json", []byte("completed"))
	})
	if err == nil {
		t.Fatal("publication to directory succeeded")
	}
	if _, err := os.Stat(filepath.Join(StateDirectory(path), commitName)); err != nil {
		t.Fatal("lost recovery intent", err)
	}
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	if err := Transact(t.Context(), path, func(tx *Transaction) error {
		data, err := tx.Read(Settings)
		if err != nil || string(data) != "installed" {
			t.Fatal(data, err)
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
}

func TestOrdinarySettingsReadRequiresNoWritableState(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.json")
	if err := Write(path, []byte("settings")); err != nil {
		t.Fatal(err)
	}
	data, err := ReadSettings(t.Context(), path)
	if err != nil || string(data) != "settings" {
		t.Fatal(data, err)
	}
	if _, err := os.Stat(path + ".lock"); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("read created lock", err)
	}
	if _, err := os.Stat(StateDirectory(path)); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("read created runtime state", err)
	}
}
