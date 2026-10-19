// Package shareconfig owns persistent frontend and relay choices for sharing.
package shareconfig

import (
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"

	"github.com/windshare/windshare/connectivity/relayset"
	"github.com/windshare/windshare/connectivity/senderrelay"
)

const (
	PathEnvironment = "WINDSHARE_CONFIG"
	maxConfigBytes  = 64 * 1024
)

var ErrConfig = errors.New("invalid sharing configuration")

// A relay-only export can omit FrontURL. Sharing requires an explicit frontend
// before starting any network work; a relay address does not imply a website.
type Config struct {
	FrontURL string
	Relays   []senderrelay.Candidate
}

// Disk credentials are decoded explicitly rather than making the engine's
// candidate serializable with its private registration authority.
type document struct {
	FrontURL string       `json:"front_url,omitempty"`
	Relays   []savedRelay `json:"relays"`
}

type savedRelay struct {
	URL               string `json:"url"`
	RegistrationToken string `json:"registration_token,omitempty"`
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
	file, err := os.Open(path)
	if !explicit && errors.Is(err, os.ErrNotExist) {
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
	var config document
	if err := decoder.Decode(&config); err != nil {
		return Config{}, ErrConfig
	}
	var trailing any
	if err := decoder.Decode(&trailing); err != io.EOF || limited.N == 0 {
		return Config{}, ErrConfig
	}
	candidates := make([]senderrelay.Candidate, 0, len(config.Relays))
	for _, relay := range config.Relays {
		candidates = append(candidates, senderrelay.Candidate{
			PublicURL: relay.URL, RegistrationToken: relay.RegistrationToken,
		})
	}
	result := Config{FrontURL: config.FrontURL, Relays: candidates}
	if err := result.Validate(); err != nil {
		return Config{}, err
	}
	return result, nil
}

func (config Config) Validate() error {
	if config.FrontURL != "" && ValidateFrontURL(config.FrontURL) != nil {
		return ErrConfig
	}
	if len(config.Relays) == 0 || len(config.Relays) > relayset.MaximumEndpoints {
		return ErrConfig
	}
	if _, err := senderrelay.PublicURLs(config.Relays); err != nil {
		return ErrConfig
	}
	return nil
}
