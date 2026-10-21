package relayconfig

import (
	"bytes"
	"encoding/json"
)

const (
	privateFileMode = 0o600
)

// Save replaces the entire allowed set. Merging would silently retain providers
// that a newly imported private-only configuration deliberately excludes.
func Save(config Config) error {
	return (FileStore{}).Save(config)
}

func (config Config) document() document {
	disk := document{ManagedTURNPolicies: config.ManagedTURNPolicies, FrontURL: config.FrontURL, Profiles: make([]savedRelay, 0, len(config.Profiles)), ActiveRelays: config.ActiveRelays}
	if len(config.TURN.Accounts) > 0 || len(config.TURN.Profiles) > 0 {
		disk.TURN = &config.TURN
	}
	for _, candidate := range config.Profiles {
		disk.Profiles = append(disk.Profiles, savedRelay{
			URL: candidate.PublicURL, AccessKey: candidate.AccessKey,
		})
	}
	return disk
}

func encode(config Config) ([]byte, error) {
	if err := config.validateFields(); err != nil {
		return nil, err
	}
	var buffer bytes.Buffer
	encoder := json.NewEncoder(&buffer)
	// Tokens are opaque ASCII, not HTML. Escaping them could expand a valid
	// imported document beyond the same reader's bounded configuration budget.
	encoder.SetEscapeHTML(false)
	if err := encoder.Encode(config.document()); err != nil {
		return nil, err
	}
	// Keep a document accepted at the input limit readable after serialization.
	buffer.Truncate(buffer.Len() - 1)
	if buffer.Len() > maxConfigBytes {
		return nil, ErrConfig
	}
	return buffer.Bytes(), nil
}

// Encode serializes only portable user settings.
func Encode(config Config) ([]byte, error) { return encode(config) }
