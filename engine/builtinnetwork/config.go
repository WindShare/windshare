// Package builtinnetwork is application wiring, never an engine default.
package builtinnetwork

import (
	"bytes"
	"crypto/ed25519"
	"encoding/base64"
	"encoding/json"
	"errors"

	"github.com/windshare/windshare/core/nodedirectory"
	"github.com/windshare/windshare/core/serviceauthority"
	release "github.com/windshare/windshare/deploy/network"
	"github.com/windshare/windshare/engine/account"
	"github.com/windshare/windshare/engine/networkconfig"
	"github.com/windshare/windshare/engine/networkdirectory"
	"github.com/windshare/windshare/engine/relayconfig"
)

type releaseConfig struct {
	Authority *struct {
		URL  string `json:"url"`
		Root string `json:"root"`
	} `json:"authority"`
	Network               string   `json:"network"`
	LinkHost              string   `json:"link_host"`
	BackupEntry           string   `json:"backup_entry"`
	BootstrapNodes        []string `json:"bootstrap_nodes"`
	Mirrors               []string `json:"mirrors"`
	NativeFallbackMirrors []string `json:"native_fallback_mirrors"`
	Roots                 []struct {
		ID  string `json:"id"`
		Key string `json:"key"`
	} `json:"roots"`
	Snapshot                 json.RawMessage `json:"snapshot"`
	GlobalPairCoversMainland bool            `json:"global_pair_covers_mainland"`
}

func Config(configPath string) (*networkconfig.Config, error) {
	return Parse(release.Release, configPath)
}

// Parse decodes and verifies an explicit release manifest with the same root
// and snapshot checks as Config. End-user applications must continue calling
// Config so compiled-in trust cannot be overridden at runtime.
func Parse(data []byte, configPath string) (*networkconfig.Config, error) {
	var source releaseConfig
	if err := json.Unmarshal(data, &source); err != nil {
		return nil, err
	}
	provisioned := len(source.Roots) != 0
	if provisioned && (len(source.Roots) != 2 || source.Roots[0].ID == source.Roots[1].ID || source.Roots[0].Key == source.Roots[1].Key) {
		return nil, errors.New("built-in network requires two distinct roots")
	}
	if err := relayconfig.ValidateBootstrapNodes(source.BootstrapNodes); err != nil {
		return nil, err
	}
	result := &networkconfig.Config{CommunitySettingsPath: configPath, BootstrapNodes: source.BootstrapNodes, LinkHost: source.LinkHost, BackupEntry: source.BackupEntry, GlobalPairCoversMainland: source.GlobalPairCoversMainland,
		Directory: networkdirectory.Config{Network: source.Network, Mirrors: source.Mirrors, FallbackMirrors: source.NativeFallbackMirrors}}
	if configPath != "" {
		result.Directory.Cache = networkdirectory.NewFileCache(configPath, source.Network)
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
		result.Directory.FallbackMirrors = nil
	}
	if source.Authority != nil {
		root, err := serviceauthority.PublicKey(source.Authority.Root)
		if err != nil {
			return nil, err
		}
		result.Authority = &account.Config{URL: source.Authority.URL, Root: root}
		if configPath != "" {
			result.Authority.Store = account.FileStore{Path: configPath + ".account.json"}
		}
	}
	return result, nil
}
