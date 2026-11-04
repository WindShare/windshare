package platformsetup

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
)

const PreferenceEnvironment = "WINDSHARE_NETWORK_PREFERENCE"
const PreferenceFileName = "network-preference.json"
const preferenceSchema = 2

// Configure records a completed rule write for one installation, not live policy.
// Skip belongs to the user and deliberately has no installation identity.
type Preference struct {
	Choice     Choice
	Executable string
}

func (p Preference) Valid() bool {
	return p.Choice.Valid() && (p.Choice == Configure && p.Executable != "" ||
		p.Choice != Configure && p.Executable == "")
}

type preferenceRecord struct {
	Schema     int    `json:"schema"`
	Choice     string `json:"choice"`
	Executable string `json:"executable,omitempty"`
}

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
	if local := os.Getenv("LOCALAPPDATA"); local != "" {
		directory = local
	}
	return filepath.Join(directory, "WindShare", PreferenceFileName), nil
}

func (store FilePreferences) Load() (Preference, error) {
	path, err := store.path()
	if err != nil {
		return Preference{}, err
	}
	data, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return Preference{}, nil
	}
	if err != nil {
		return Preference{}, err
	}
	var record preferenceRecord
	if json.Unmarshal(data, &record) != nil || record.Schema != preferenceSchema {
		return Preference{}, errors.New("invalid network preference")
	}
	for _, choice := range []Choice{Ask, Configure, Skip} {
		preference := Preference{Choice: choice, Executable: record.Executable}
		if record.Choice == choice.String() && preference.Valid() {
			return preference, nil
		}
	}
	return Preference{}, errors.New("invalid network preference choice or executable")
}

func (store FilePreferences) Save(preference Preference) error {
	if !preference.Valid() {
		return errors.New("invalid network preference")
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
	writeErr := json.NewEncoder(file).Encode(preferenceRecord{preferenceSchema, preference.Choice.String(), preference.Executable})
	if err := errors.Join(writeErr, file.Close()); err != nil {
		return err
	}
	return os.Rename(file.Name(), path)
}
