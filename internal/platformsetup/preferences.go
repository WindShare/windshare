package platformsetup

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
)

const PreferenceEnvironment = "WINDSHARE_NETWORK_PREFERENCE"
const PreferenceFileName = "network-preference.json"
const preferenceSchema = 1

type FilePreferences struct{ Path string }

func (store FilePreferences) path() (string, error) {
	if store.Path != "" {
		return store.Path, nil
	}
	if path := os.Getenv(PreferenceEnvironment); path != "" {
		return path, nil
	}
	directory, err := os.UserConfigDir()
	if err != nil {
		return "", err
	}
	// Installation preferences belong to the user, not an executable location or
	// network identity. Moving the binary must not resurrect dismissed reminders.
	if local := os.Getenv("LOCALAPPDATA"); local != "" {
		directory = local
	}
	return filepath.Join(directory, "WindShare", PreferenceFileName), nil
}

func (store FilePreferences) Load() (Choice, error) {
	path, err := store.path()
	if err != nil {
		return Ask, err
	}
	data, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return Ask, nil
	}
	if err != nil {
		return Ask, err
	}
	var record struct {
		Schema int    `json:"schema"`
		Choice string `json:"choice"`
	}
	if json.Unmarshal(data, &record) != nil || record.Schema != preferenceSchema {
		return Ask, errors.New("invalid network preference")
	}
	for _, choice := range []Choice{Ask, Configure, Skip} {
		if record.Choice == choice.String() {
			return choice, nil
		}
	}
	return Ask, errors.New("invalid network preference choice")
}

func (store FilePreferences) Save(choice Choice) error {
	if !choice.Valid() {
		return errors.New("invalid network preference choice")
	}
	path, err := store.path()
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	file, err := os.CreateTemp(filepath.Dir(path), ".network-preference-*")
	if err != nil {
		return err
	}
	defer func() { _ = os.Remove(file.Name()) }()
	_, writeErr := fmt.Fprintf(file, "{\"schema\":%d,\"choice\":%q}\n", preferenceSchema, choice.String())
	if err := errors.Join(writeErr, file.Close()); err != nil {
		return err
	}
	return os.Rename(file.Name(), path)
}
