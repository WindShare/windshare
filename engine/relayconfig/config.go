// Package relayconfig owns saved profiles, active relay selection and frontend choice.
package relayconfig

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"

	"github.com/windshare/windshare/connectivity/relayset"
	"github.com/windshare/windshare/engine/internal/statefile"
	"github.com/windshare/windshare/engine/relayprofile"
	"github.com/windshare/windshare/engine/turn"
	v2 "github.com/windshare/windshare/relay/protocol/v2"
)

const (
	PathEnvironment = "WINDSHARE_CONFIG"
	maxConfigBytes  = 64 * 1024
	MaximumProfiles = 64
)

var ErrConfig = errors.New("invalid sharing configuration")

type Config struct {
	ManagedTURNPolicies []turn.ManagedPolicy
	TURN                turn.ManualSettings
	FrontURL            string
	Profiles            []relayprofile.Candidate
	ActiveRelays        []string
}

// TURNSettings projects only authority and policy; unrelated application settings
// cannot become a dependency of runtime TURN configuration.
func (config Config) TURNSettings() turn.Settings {
	return (turn.Settings{Manual: config.TURN, Managed: turn.ManagedSettings{
		Relays: config.Profiles, Policies: config.ManagedTURNPolicies,
	}}).Clone()
}

type document struct {
	ManagedTURNPolicies []turn.ManagedPolicy `json:"managed_turn_policies,omitempty"`
	TURN                *turn.ManualSettings `json:"turn,omitempty"`
	FrontURL            string               `json:"front_url,omitempty"`
	Profiles            []savedRelay         `json:"profiles"`
	ActiveRelays        []string             `json:"active_relays,omitempty"`
}
type savedRelay struct {
	URL       string `json:"url"`
	AccessKey string `json:"access_key,omitempty"`
}

func configurationPath() (path string, explicit bool, err error) {
	if path, explicit = os.LookupEnv(PathEnvironment); explicit {
		return path, true, nil
	}
	directory, err := os.UserConfigDir()
	if err != nil {
		return "", false, err
	}
	return filepath.Join(directory, "windshare", "config.json"), false, nil
}

func Load() (Config, error) {
	path, explicit, err := configurationPath()
	if err != nil {
		return Config{}, err
	}
	return loadPath(path, !explicit)
}
func loadPath(path string, missingAllowed bool) (Config, error) {
	data, err := statefile.ReadSettings(context.Background(), path)
	if missingAllowed && errors.Is(err, os.ErrNotExist) {
		return Config{}, nil
	}
	if err != nil {
		return Config{}, err
	}
	return Decode(bytes.NewReader(data))
}

func Decode(reader io.Reader) (Config, error) {
	limited := &io.LimitedReader{R: reader, N: maxConfigBytes + 1}
	decoder := json.NewDecoder(limited)
	decoder.DisallowUnknownFields()
	var disk document
	if err := decoder.Decode(&disk); err != nil {
		return Config{}, ErrConfig
	}
	var trailing any
	if err := decoder.Decode(&trailing); err != io.EOF || limited.N == 0 {
		return Config{}, ErrConfig
	}
	result := Config{ManagedTURNPolicies: disk.ManagedTURNPolicies, FrontURL: disk.FrontURL, ActiveRelays: disk.ActiveRelays}
	if disk.TURN != nil {
		result.TURN = *disk.TURN
	}
	if disk.Profiles != nil {
		result.Profiles = make([]relayprofile.Candidate, 0, len(disk.Profiles))
	}
	for _, profile := range disk.Profiles {
		result.Profiles = append(result.Profiles, relayprofile.Candidate{PublicURL: profile.URL, AccessKey: profile.AccessKey})
	}
	if err := result.Validate(); err != nil {
		return Config{}, err
	}
	return result, nil
}
func (config Config) Validate() error {
	_, err := encode(config)
	return err
}

func (config Config) validateFields() error {
	if err := config.TURNSettings().Validate(); err != nil {
		return errors.Join(ErrConfig, err)
	}
	if config.FrontURL != "" && ValidateFrontURL(config.FrontURL) != nil {
		return ErrConfig
	}
	if config.Profiles == nil || len(config.Profiles) > MaximumProfiles ||
		len(config.ActiveRelays) > relayset.MaximumEndpoints {
		return ErrConfig
	}
	if _, err := relayprofile.PublicURLs(config.Profiles); err != nil {
		return ErrConfig
	}
	saved := map[string]bool{}
	for _, profile := range config.Profiles {
		endpoint, _ := v2.NormalizeRelayEndpoint(profile.PublicURL)
		saved[endpoint.DialURL] = true
	}
	seen := map[string]bool{}
	for _, address := range config.ActiveRelays {
		endpoint, err := v2.NormalizeRelayEndpoint(address)
		if err != nil || !saved[endpoint.DialURL] || seen[endpoint.DialURL] {
			return ErrConfig
		}
		seen[endpoint.DialURL] = true
	}
	return nil
}

// FileStore is injectable so applications never read another caller's configuration implicitly.
type FileStore struct{ Path string }

func (store FileStore) Load() (Config, error) {
	if store.Path == "" {
		path, _, err := configurationPath()
		if err != nil {
			return Config{}, err
		}
		return loadPath(path, true)
	}
	return loadPath(store.Path, true)
}
func (store FileStore) Save(config Config) error {
	if err := config.Validate(); err != nil {
		return err
	}
	return store.save(config)
}

// Location identifies the namespace shared by settings and durable setup commits.
func (store FileStore) Location() (string, error) {
	if store.Path != "" {
		return store.Path, nil
	}
	path, _, err := configurationPath()
	return path, err
}
