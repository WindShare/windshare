// Package relayconfig owns the sender's private, local relay selection.
package relayconfig

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
	PathEnvironment = "WINDSHARE_RELAY_CONFIG"
	maxConfigBytes  = 64 * 1024
)

var ErrConfig = errors.New("invalid relay configuration")

// Disk credentials are decoded explicitly rather than making the engine's
// candidate serializable with its private registration authority.
type document struct {
	Relays []savedRelay `json:"relays"`
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
	return filepath.Join(directory, "windshare", "relays.json"), false, nil
}

func Load() ([]senderrelay.Candidate, error) {
	path, explicit, err := configurationPath()
	if err != nil {
		return nil, err
	}
	file, err := os.Open(path)
	if !explicit && errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	defer file.Close()
	return Decode(file)
}

func Decode(reader io.Reader) ([]senderrelay.Candidate, error) {
	limited := &io.LimitedReader{R: reader, N: maxConfigBytes + 1}
	decoder := json.NewDecoder(limited)
	decoder.DisallowUnknownFields()
	var config document
	if err := decoder.Decode(&config); err != nil {
		return nil, ErrConfig
	}
	var trailing any
	if err := decoder.Decode(&trailing); err != io.EOF || limited.N == 0 {
		return nil, ErrConfig
	}
	candidates := make([]senderrelay.Candidate, 0, len(config.Relays))
	for _, relay := range config.Relays {
		candidates = append(candidates, senderrelay.Candidate{
			PublicURL: relay.URL, RegistrationToken: relay.RegistrationToken,
		})
	}
	if err := validate(candidates); err != nil {
		return nil, err
	}
	return candidates, nil
}

func validate(candidates []senderrelay.Candidate) error {
	if len(candidates) == 0 || len(candidates) > relayset.MaximumEndpoints {
		return ErrConfig
	}
	if _, err := senderrelay.PublicURLs(candidates); err != nil {
		return ErrConfig
	}
	return nil
}
