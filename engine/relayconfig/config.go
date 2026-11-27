// Package relayconfig owns saved profiles, active relay selection and frontend choice.
package relayconfig

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"

	"github.com/windshare/windshare/connectivity/publicdial"
	"github.com/windshare/windshare/connectivity/relayset"
	"github.com/windshare/windshare/engine/internal/configjson"
	"github.com/windshare/windshare/engine/internal/statefile"
	"github.com/windshare/windshare/engine/relayprofile"
	v2 "github.com/windshare/windshare/relay/protocol/v2"
)

const (
	PathEnvironment       = "WINDSHARE_CONFIG"
	maxConfigBytes        = 64 * 1024
	MaximumProfiles       = 64
	MaximumBootstrapNodes = 16
)

var (
	ErrConfig   = errors.New("invalid sharing configuration")
	ErrRelayURL = errors.New("invalid relay URL")
	// Decode wraps both ErrConfig and ErrRepeatedMember for ambiguous names.
	ErrRepeatedMember = configjson.ErrRepeatedMember
)

type Config struct {
	BootstrapNodes []string
	TURNServices   []TURNService
	FrontURL       string
	Profiles       []relayprofile.Candidate
	ActiveRelays   []string
	CommunityNodes *bool
	ConfiguredOnly bool
}

// Missing settings are an editable configuration, with the same explicit empty
// profile set that the persisted format requires. Malformed documents still fail.
func emptyConfig() Config { return Config{Profiles: []relayprofile.Candidate{}} }

type document struct {
	BootstrapNodes []string      `json:"bootstrap_nodes,omitempty"`
	TURNServices   []TURNService `json:"turn_services,omitempty"`
	FrontURL       string        `json:"front_url,omitempty"`
	Profiles       []savedRelay  `json:"profiles"`
	ActiveRelays   []string      `json:"active_relays,omitempty"`
	CommunityNodes *bool         `json:"community_nodes,omitempty"`
	ConfiguredOnly bool          `json:"configured_relays_only,omitempty"`
}
type savedRelay struct {
	Node            relayprofile.NodeIdentity   `json:"node,omitzero"`
	Role            relayprofile.CredentialRole `json:"credential_role,omitempty"`
	ManagedTurn     bool                        `json:"managed_turn,omitempty"`
	URL             string                      `json:"url"`
	ConsoleURL      string                      `json:"console_url,omitempty"`
	OfferedFrontend string                      `json:"offered_frontend,omitempty"`
	AccessKey       string                      `json:"access_key,omitempty"`
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

func Path() (string, error) { path, _, err := configurationPath(); return path, err }

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
		return emptyConfig(), nil
	}
	if err != nil {
		return Config{}, err
	}
	return decodeSaved(path, data)
}

func decodeSaved(path string, data []byte) (Config, error) {
	config, err := Decode(bytes.NewReader(data))
	if errors.Is(err, ErrRepeatedMember) || errors.Is(err, ErrRelayURL) {
		// Report the file used by this operation, including injected/relative paths.
		// Editing through a read-modify-write cannot repair a rejected document.
		if absolute, pathErr := filepath.Abs(path); pathErr == nil {
			path = absolute
		}
		return Config{}, fmt.Errorf("%w; edit %+q and retry", err, path)
	}
	return config, err
}

func Decode(reader io.Reader) (Config, error) {
	data, err := io.ReadAll(io.LimitReader(reader, maxConfigBytes+1))
	if err != nil || len(data) > maxConfigBytes {
		return Config{}, ErrConfig
	}
	var disk document
	if err := configjson.Decode(data, &disk); err != nil {
		if errors.Is(err, ErrRepeatedMember) {
			return Config{}, fmt.Errorf("%w; %w", ErrConfig, err)
		}
		return Config{}, ErrConfig
	}
	result := Config{BootstrapNodes: disk.BootstrapNodes, TURNServices: disk.TURNServices, FrontURL: disk.FrontURL, ActiveRelays: disk.ActiveRelays, CommunityNodes: disk.CommunityNodes, ConfiguredOnly: disk.ConfiguredOnly}
	if disk.Profiles != nil {
		result.Profiles = make([]relayprofile.Candidate, 0, len(disk.Profiles))
	}
	for _, profile := range disk.Profiles {
		result.Profiles = append(result.Profiles, relayprofile.Candidate{Role: profile.Role, Node: profile.Node, ManagedTurn: profile.ManagedTurn, PublicURL: profile.URL, AccessKey: profile.AccessKey, ConsoleURL: profile.ConsoleURL, OfferedFrontend: profile.OfferedFrontend})
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
	if len(config.BootstrapNodes) > MaximumBootstrapNodes {
		return fmt.Errorf("%w: too many bootstrap nodes", ErrConfig)
	}
	for _, seed := range config.BootstrapNodes {
		if _, err := (publicdial.Policy{}).Origin(seed); err != nil {
			return fmt.Errorf("%w: invalid bootstrap node", ErrConfig)
		}
	}
	if err := ValidateTURNServices(config.TURNServices); err != nil {
		return err
	}
	if config.FrontURL != "" {
		if err := ValidateFrontURL(config.FrontURL); err != nil {
			return fmt.Errorf("%w: %w", ErrConfig, err)
		}
	}
	if config.Profiles == nil || len(config.Profiles) > MaximumProfiles ||
		len(config.ActiveRelays) > relayset.MaximumEndpoints {
		return ErrConfig
	}
	saved := map[string]bool{}
	for index, profile := range config.Profiles {
		endpoint, err := v2.NormalizeRelayEndpoint(profile.PublicURL)
		if err != nil {
			return fmt.Errorf("%w: profiles[%d].url: %w", ErrConfig, index, relayAddressError{cause: err})
		}
		for _, page := range []string{profile.ConsoleURL, profile.OfferedFrontend} {
			if page != "" && ValidateFrontURL(page) != nil {
				return fmt.Errorf("%w: invalid relay management or frontend address", ErrConfig)
			}
		}
		saved[endpoint.DialURL] = true
	}
	if _, err := relayprofile.PublicURLs(config.Profiles); err != nil {
		return ErrConfig
	}
	seen := map[string]bool{}
	for index, address := range config.ActiveRelays {
		endpoint, err := v2.NormalizeRelayEndpoint(address)
		if err != nil {
			return fmt.Errorf("%w: active_relays[%d]: %w", ErrConfig, index, relayAddressError{cause: err})
		}
		if !saved[endpoint.DialURL] || seen[endpoint.DialURL] {
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
