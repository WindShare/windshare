package relayconfig

import (
	"bytes"
	"crypto/rand"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
)

const (
	privateDirectoryMode = 0o700
	privateFileMode      = 0o600
	stagingFilePrefix    = ".config-import-"
)

// Save replaces the entire allowed set. Merging would silently retain providers
// that a newly imported private-only configuration deliberately excludes.
func Save(config Config) error {
	return (FileStore{}).Save(config)
}

func encode(config Config) ([]byte, error) {
	if err := config.validateFields(); err != nil {
		return nil, err
	}
	disk := document{FrontURL: config.FrontURL, Profiles: make([]savedRelay, 0, len(config.Profiles)), ActiveRelays: config.ActiveRelays, Pending: config.Pending}
	if len(config.TURN.Accounts) > 0 || len(config.TURN.Profiles) > 0 {
		disk.TURN = &config.TURN
	}
	for _, candidate := range config.Profiles {
		disk.Profiles = append(disk.Profiles, savedRelay{
			URL: candidate.PublicURL, AccessKey: candidate.AccessKey,
		})
	}
	var buffer bytes.Buffer
	encoder := json.NewEncoder(&buffer)
	// Tokens are opaque ASCII, not HTML. Escaping them could expand a valid
	// imported document beyond the same reader's bounded configuration budget.
	encoder.SetEscapeHTML(false)
	if err := encoder.Encode(disk); err != nil {
		return nil, err
	}
	// Keep a document accepted at the input limit readable after serialization.
	buffer.Truncate(buffer.Len() - 1)
	if buffer.Len() > maxConfigBytes {
		return nil, ErrConfig
	}
	return buffer.Bytes(), nil
}

func replaceConfiguration(path string, data []byte) (err error) {
	if path == "" {
		return os.ErrInvalid
	}
	info, err := os.Lstat(path)
	if err == nil && !info.Mode().IsRegular() {
		return errors.New("sharing configuration destination must be a regular file")
	}
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	directory := filepath.Dir(path)
	if err := os.MkdirAll(directory, privateDirectoryMode); err != nil {
		return err
	}
	// A sibling staging file permits one filesystem rename to publish a complete
	// selection. The existing file is never truncated or removed before commit.
	staging := filepath.Join(directory, stagingFilePrefix+rand.Text())
	file, err := createPrivateFile(staging)
	if err != nil {
		return err
	}
	defer func() {
		if cleanupErr := os.Remove(staging); cleanupErr != nil && !errors.Is(cleanupErr, os.ErrNotExist) {
			err = errors.Join(err, cleanupErr)
		}
	}()
	defer file.Close()
	if _, err := file.Write(data); err != nil {
		return err
	}
	if err := file.Sync(); err != nil {
		return err
	}
	if err := file.Close(); err != nil {
		return err
	}
	return replaceFile(staging, path)
}
