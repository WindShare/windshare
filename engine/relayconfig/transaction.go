package relayconfig

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"time"

	"github.com/windshare/windshare/connectivity/senderrelay"
)

const configurationLockRetry = 10 * time.Millisecond

// SettingsRevision excludes workflow checkpoints so approvals can advance
// without invalidating themselves, while a changed import supersedes old intent.
func (config Config) SettingsRevision() string {
	settings := config
	settings.Pending = nil
	// Discovery inventory is not an in-progress onboarding decision.
	settings.ManagedTURN = nil
	if len(settings.Profiles) == 0 {
		settings.Profiles = []senderrelay.Candidate{}
	}
	if len(settings.ActiveRelays) == 0 {
		settings.ActiveRelays = nil
	}
	data, _ := json.Marshal(settings)
	digest := sha256.Sum256(data)
	return base64.RawURLEncoding.EncodeToString(digest[:])
}

// Update serializes a short read/modify/commit across all native callers.
// The callback must contain no network calls or user interaction.
func (store FileStore) Update(ctx context.Context, change func(*Config) error) (Config, error) {
	var committed Config
	err := store.withLock(ctx, func(path string) error {
		current, err := loadPath(path, true)
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
		if err := replaceConfiguration(path, data); err != nil {
			return err
		}
		committed = current
		return nil
	})
	return committed, err
}

func (store FileStore) replaceSettings(config Config) error {
	// Imported workflow state cannot replace a local claimant or discard the
	// information needed to cancel an issued credential.
	config.Pending = nil
	if err := config.Validate(); err != nil {
		return err
	}
	return store.withLock(context.Background(), func(path string) error {
		current, err := loadPath(path, true)
		if err != nil && !errors.Is(err, ErrConfig) {
			return err
		}
		config.Pending = current.Pending
		data, err := encode(config)
		if err != nil {
			return err
		}
		return replaceConfiguration(path, data)
	})
}

func (store FileStore) withLock(ctx context.Context, action func(string) error) error {
	path := store.Path
	if path == "" {
		var err error
		path, _, err = configurationPath()
		if err != nil {
			return err
		}
	}
	if path == "" {
		return os.ErrInvalid
	}
	if info, err := os.Lstat(path); err == nil && !info.Mode().IsRegular() {
		return errors.New("sharing configuration destination must be a regular file")
	} else if err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(path), privateDirectoryMode); err != nil {
		return err
	}
	// This inode stays in place: removing a lock file lets another process lock
	// a replacement while a previous waiter still owns the original inode.
	file, err := os.OpenFile(path+".lock", os.O_CREATE|os.O_RDWR, privateFileMode)
	if err != nil {
		return err
	}
	defer file.Close()
	for {
		if err := ctx.Err(); err != nil {
			return err
		}
		locked, err := tryConfigurationLock(file)
		if err != nil {
			return err
		}
		if locked {
			defer releaseConfigurationLock(file)
			return action(path)
		}
		timer := time.NewTimer(configurationLockRetry)
		select {
		case <-ctx.Done():
			timer.Stop()
			return ctx.Err()
		case <-timer.C:
		}
	}
}
