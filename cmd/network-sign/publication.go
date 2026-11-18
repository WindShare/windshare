package main

import (
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"flag"
	"os"
	"time"

	"github.com/windshare/windshare/core/nodedirectory"
)

// The console can compare public mirror bytes with this authenticated release
// without giving the management Worker a directory signing key.
func publication(args []string) error {
	flags := flag.NewFlagSet("publication", flag.ContinueOnError)
	path := flags.String("release", "deploy/network/release.json", "network release manifest")
	if err := flags.Parse(args); err != nil {
		return err
	}
	if flags.NArg() != 0 {
		return nodedirectory.ErrFormat
	}
	var manifest releaseManifest
	if err := readJSON(*path, &manifest); err != nil {
		return err
	}
	if len(manifest.Roots) == 0 && string(manifest.Snapshot) == "null" {
		return json.NewEncoder(os.Stdout).Encode(nil)
	}
	roots, err := releaseRoots(manifest)
	if err != nil {
		return err
	}
	snapshot, err := nodedirectory.Verify(manifest.Snapshot, manifest.Network, roots)
	if err != nil {
		return err
	}
	if err := nodedirectory.Accept(nodedirectory.Cursor{}, snapshot, time.Now(), 0); err != nil {
		if errors.Is(err, nodedirectory.ErrExpired) {
			return json.NewEncoder(os.Stdout).Encode(nil)
		}
		return err
	}
	var e nodedirectory.Envelope
	if err := json.Unmarshal(snapshot.Envelope(), &e); err != nil {
		return err
	}
	values, err := json.Marshal([]string{e.Certificate.Root, e.Certificate.Payload, e.Certificate.Signature, e.Payload, e.Signature})
	if err != nil {
		return err
	}
	digest := sha256.Sum256(values)
	payload := snapshot.Payload()
	keys := []string{}
	for _, node := range payload.Nodes {
		if node.HasRole("relay") {
			keys = append(keys, node.NodeKey)
		}
	}
	return json.NewEncoder(os.Stdout).Encode(struct {
		Digest     string   `json:"digest"`
		Generation uint64   `json:"generation"`
		Serial     uint64   `json:"serial"`
		Expires    int64    `json:"expires"`
		Keys       []string `json:"keys"`
	}{base64.RawURLEncoding.EncodeToString(digest[:]), payload.Generation, payload.Serial, snapshot.TrustedUntil(0).Unix(), keys})
}
