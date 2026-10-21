// Package turncache stores replaceable observations, scoped to relay authority.
package turncache

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"time"

	"github.com/windshare/windshare/connectivity/senderrelay"
	"github.com/windshare/windshare/engine/internal/statefile"
	"github.com/windshare/windshare/engine/relayconfig"
	"github.com/windshare/windshare/engine/turn"
	v2 "github.com/windshare/windshare/relay/protocol/v2"
)

const maximumRecordBytes = 4096

type FileStore struct{ Settings relayconfig.FileStore }
type observation struct {
	RelayURL    string          `json:"relay_url"`
	Authority   string          `json:"authority"`
	Capability  turn.Capability `json:"capability"`
	RequestedAt time.Time       `json:"requested_at"`
}

func digest(value string) string {
	hash := sha256.Sum256([]byte(value))
	return hex.EncodeToString(hash[:])
}
func (store FileStore) location(candidate senderrelay.Candidate) (string, string, error) {
	endpoint, err := v2.NormalizeRelayEndpoint(candidate.PublicURL)
	if err != nil {
		return "", "", err
	}
	settings, err := store.Settings.Location()
	if err != nil {
		return "", "", err
	}
	return filepath.Join(statefile.StateDirectory(settings), "turn", digest(endpoint.DialURL)+".json"), endpoint.DialURL, nil
}
func read(path string) (*observation, error) {
	file, err := os.Open(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	defer file.Close()
	data, err := io.ReadAll(io.LimitReader(file, maximumRecordBytes+1))
	if err != nil {
		return nil, err
	}
	if len(data) > maximumRecordBytes {
		return nil, errors.New("TURN cache record too large")
	}
	var result observation
	if err := json.Unmarshal(data, &result); err != nil {
		return nil, err
	}
	if result.Capability.Validate() != nil || result.RequestedAt.IsZero() {
		return nil, errors.New("invalid TURN cache observation")
	}
	return &result, nil
}
func (store FileStore) Load(candidate senderrelay.Candidate) (*turn.Capability, error) {
	path, address, err := store.location(candidate)
	if err != nil {
		return nil, err
	}
	record, err := read(path)
	if err != nil || record == nil {
		return nil, err
	}
	if record.RelayURL != address || record.Authority != digest(candidate.AccessKey) {
		return nil, nil
	}
	return &record.Capability, nil
}
func (store FileStore) Save(ctx context.Context, candidate senderrelay.Candidate, capability turn.Capability, requestedAt time.Time) error {
	if capability.Validate() != nil || candidate.AccessKey == "" || requestedAt.IsZero() {
		return turn.ErrConfiguration
	}
	path, address, err := store.location(candidate)
	if err != nil {
		return err
	}
	return statefile.WithLock(ctx, path, func() error {
		// An older request from another Engine must not resurrect a later withdrawal.
		// Corrupt optional cache data is replaced, independently of user settings.
		previous, _ := read(path)
		if previous != nil && previous.RequestedAt.After(requestedAt) {
			return nil
		}
		data, err := json.Marshal(observation{RelayURL: address, Authority: digest(candidate.AccessKey), Capability: capability, RequestedAt: requestedAt})
		if err != nil {
			return err
		}
		return statefile.Write(path, data)
	})
}
