// Package builtinnetwork is application wiring, never an engine default.
package builtinnetwork

import (
	"bytes"
	"crypto/ed25519"
	"encoding/base64"
	"encoding/json"
	"errors"

	"github.com/windshare/windshare/core/nodedirectory"
	release "github.com/windshare/windshare/deploy/network"
	"github.com/windshare/windshare/engine/networkconfig"
	"github.com/windshare/windshare/engine/networkdirectory"
)

type releaseConfig struct {
	Network     string   `json:"network"`
	LinkHost    string   `json:"link_host"`
	BackupEntry string   `json:"backup_entry"`
	Mirrors     []string `json:"mirrors"`
	Roots       []struct {
		ID  string `json:"id"`
		Key string `json:"key"`
	} `json:"roots"`
	Snapshot                 json.RawMessage `json:"snapshot"`
	GlobalPairCoversMainland bool            `json:"global_pair_covers_mainland"`
}

func Config(configPath string) (*networkconfig.Config, error) {
	return parse(release.Release, configPath)
}

func parse(data []byte, configPath string) (*networkconfig.Config, error) {
	var source releaseConfig
	if err := json.Unmarshal(data, &source); err != nil {
		return nil, err
	}
	provisioned := len(source.Roots) != 0
	if provisioned && (len(source.Roots) != 2 || source.Roots[0].ID == source.Roots[1].ID || source.Roots[0].Key == source.Roots[1].Key) {
		return nil, errors.New("built-in network requires two distinct roots")
	}
	result := &networkconfig.Config{LinkHost: source.LinkHost, BackupEntry: source.BackupEntry, GlobalPairCoversMainland: source.GlobalPairCoversMainland,
		Directory: networkdirectory.Config{Network: source.Network, Mirrors: source.Mirrors}}
	if configPath != "" {
		result.Directory.Cache = networkdirectory.NewFileCache(configPath)
	}
	for _, root := range source.Roots {
		key, err := base64.RawURLEncoding.Strict().DecodeString(root.Key)
		if err != nil || len(key) != ed25519.PublicKeySize {
			return nil, errors.New("invalid built-in network root")
		}
		result.Directory.Roots = append(result.Directory.Roots, nodedirectory.Root{ID: root.ID, Key: ed25519.PublicKey(key)})
	}
	if !bytes.Equal(bytes.TrimSpace(source.Snapshot), []byte("null")) {
		result.Directory.Embedded = source.Snapshot
	}
	if provisioned || len(result.Directory.Embedded) != 0 {
		if _, err := nodedirectory.Verify(result.Directory.Embedded, source.Network, result.Directory.Roots); err != nil {
			return nil, err
		}
	}
	// Unprovisioned releases still support explicit relays. They must not trust
	// unsigned mirrors or manufacture an official signing authority on first run.
	if len(result.Directory.Roots) == 0 {
		result.Directory.Mirrors = nil
	}
	return result, nil
}
