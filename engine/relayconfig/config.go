// Package relayconfig owns saved profiles, active relay selection and frontend choice.
package relayconfig

import (
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"

	"github.com/windshare/windshare/connectivity/relayset"
	"github.com/windshare/windshare/connectivity/senderrelay"
	"github.com/windshare/windshare/engine/turnconfig"
	v2 "github.com/windshare/windshare/relay/protocol/v2"
)

const (
	PathEnvironment = "WINDSHARE_CONFIG"
	maxConfigBytes  = 64 * 1024
	MaximumProfiles = 64
)

var ErrConfig = errors.New("invalid sharing configuration")

type Config struct {
	TURN         turnconfig.Config
	FrontURL     string
	Profiles     []senderrelay.Candidate
	ActiveRelays []string
	Pending      *PendingSetup
}

// Pending setup survives process interruption without publishing a partial selection.
type PendingSetup struct {
	State            SetupState `json:"state"`
	SettingsRevision string     `json:"settings_revision"`
	OperationID      string     `json:"operation_id"`
	RelayURL         string     `json:"relay_url"`
	ConsoleURL       string     `json:"console_url"`
	FrontURL         string     `json:"front_url,omitempty"`
	DeviceName       string     `json:"device_name"`
	ClaimSecret      string     `json:"claim_secret"`
	Operation        string     `json:"operation"`
	EnrollmentID     string     `json:"enrollment_id,omitempty"`
	UserCode         string     `json:"user_code,omitempty"`
	VerificationURL  string     `json:"verification_url,omitempty"`
	ExpiresAt        int64      `json:"expires_at,omitempty"`
	IntervalMS       int64      `json:"interval_ms,omitempty"`
	AccessKey        string     `json:"access_key,omitempty"`
	CredentialID     string     `json:"credential_id,omitempty"`
}

type document struct {
	TURN         *turnconfig.Config `json:"turn,omitempty"`
	FrontURL     string             `json:"front_url,omitempty"`
	Profiles     []savedRelay       `json:"profiles"`
	ActiveRelays []string           `json:"active_relays,omitempty"`
	Pending      *PendingSetup      `json:"pending_setup,omitempty"`
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
	file, err := os.Open(path)
	if missingAllowed && errors.Is(err, os.ErrNotExist) {
		return Config{}, nil
	}
	if err != nil {
		return Config{}, err
	}
	defer file.Close()
	return Decode(file)
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
	result := Config{FrontURL: disk.FrontURL, ActiveRelays: disk.ActiveRelays, Pending: disk.Pending}
	if disk.TURN != nil {
		result.TURN = *disk.TURN
	}
	if disk.Profiles != nil {
		result.Profiles = make([]senderrelay.Candidate, 0, len(disk.Profiles))
	}
	for _, profile := range disk.Profiles {
		result.Profiles = append(result.Profiles, senderrelay.Candidate{PublicURL: profile.URL, AccessKey: profile.AccessKey})
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
	if err := config.TURN.Validate(); err != nil {
		return errors.Join(ErrConfig, err)
	}
	if config.FrontURL != "" && ValidateFrontURL(config.FrontURL) != nil {
		return ErrConfig
	}
	if (config.Profiles == nil && config.Pending == nil) || len(config.Profiles) > MaximumProfiles ||
		len(config.ActiveRelays) > relayset.MaximumEndpoints {
		return ErrConfig
	}
	if _, err := senderrelay.PublicURLs(config.Profiles); err != nil {
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
	if config.Pending != nil && !config.Pending.valid() {
		return ErrConfig
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
	return store.replaceSettings(config)
}
