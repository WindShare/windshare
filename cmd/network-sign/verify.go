package main

import (
	"bytes"
	"crypto/ed25519"
	"encoding/base64"
	"encoding/json"
	"errors"
	"flag"
	"os"
	"path/filepath"

	"github.com/windshare/windshare/core/nodedirectory"
)

func releaseRoots(manifest releaseManifest) ([]nodedirectory.Root, error) {
	if len(manifest.Roots) != 2 || manifest.Roots[0].ID == manifest.Roots[1].ID || manifest.Roots[0].Key == manifest.Roots[1].Key {
		return nil, errors.New("release must pin two independently held offline root public keys")
	}
	var roots []nodedirectory.Root
	for _, root := range manifest.Roots {
		key, err := base64.RawURLEncoding.Strict().DecodeString(root.Key)
		if err != nil || len(key) != ed25519.PublicKeySize || root.ID == "" {
			return nil, nodedirectory.ErrFormat
		}
		roots = append(roots, nodedirectory.Root{ID: root.ID, Key: ed25519.PublicKey(key)})
	}
	return roots, nil
}

func verify(args []string) error {
	flags := flag.NewFlagSet("verify", flag.ContinueOnError)
	release := flags.String("release", "deploy/network/release.json", "shared release configuration")
	mirror := flags.String("mirror", "deploy/network/windshare-network.json", "published mirror envelope")
	history := flags.String("history", "deploy/network/history", "signed export history")
	if err := flags.Parse(args); err != nil {
		return err
	}
	if flags.NArg() != 0 {
		return nodedirectory.ErrFormat
	}
	var manifest releaseManifest
	if err := readJSON(*release, &manifest); err != nil {
		return err
	}
	roots, err := releaseRoots(manifest)
	if err != nil {
		return err
	}
	snapshot, err := nodedirectory.Verify(manifest.Snapshot, manifest.Network, roots)
	if err != nil {
		return err
	}
	var embedded, exported nodedirectory.Envelope
	if err := json.Unmarshal(manifest.Snapshot, &embedded); err != nil {
		return err
	}
	if err := readJSON(*mirror, &exported); err != nil {
		return err
	}
	if embedded != exported {
		return errors.New("mirror and embedded directory have different signed bytes")
	}
	return validateHistory(*history, snapshot, manifest, roots)
}

func writeHistory(path string, data []byte) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	// Exclusive creation prevents another publisher from replacing this cursor's
	// signed bytes after history validation. Incomplete writes fail closed.
	file, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o644)
	if errors.Is(err, os.ErrExist) {
		existing, readErr := os.ReadFile(path)
		if readErr != nil {
			return readErr
		}
		if !bytes.Equal(existing, data) {
			return nodedirectory.ErrRollback
		}
		return nil
	}
	if err != nil {
		return err
	}
	defer file.Close()
	if _, err := file.Write(data); err != nil {
		return err
	}
	return file.Sync()
}
