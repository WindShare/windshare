package main

import (
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/windshare/windshare/core/nodedirectory"
)

type releaseManifest struct {
	Network                  string          `json:"network"`
	LinkHost                 string          `json:"link_host"`
	BackupEntry              string          `json:"backup_entry"`
	Mirrors                  []string        `json:"mirrors"`
	Roots                    []publicKey     `json:"roots"`
	Snapshot                 json.RawMessage `json:"snapshot"`
	GlobalPairCoversMainland bool            `json:"global_pair_covers_mainland"`
}

func sign(args []string) error {
	flags := flag.NewFlagSet("sign", flag.ContinueOnError)
	source := flags.String("source", "deploy/network/directory.json", "directory payload JSON")
	release := flags.String("release", "deploy/network/release.json", "shared native/browser release configuration")
	history := flags.String("history", "deploy/network/history", "immutable signed export history")
	keyPath := flags.String("online-private", "", "online signing PEM file")
	certPath := flags.String("certificate", "", "root-signed online certificate JSON")
	output := flags.String("out", "deploy/network/windshare-network.json", "mirror export")
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
	var payload nodedirectory.Payload
	if err := readJSON(*source, &payload); err != nil {
		return err
	}
	if payload.Network != manifest.Network {
		return nodedirectory.ErrFormat
	}
	var certificate nodedirectory.SignedCertificate
	if err := readJSON(*certPath, &certificate); err != nil {
		return err
	}
	key, err := privateKey(*keyPath)
	if err != nil {
		return err
	}
	defer clear(key)
	signed, err := nodedirectory.Sign(payload, certificate, key, roots)
	if err != nil {
		return err
	}
	next, err := nodedirectory.Verify(signed, manifest.Network, roots)
	if err != nil {
		return err
	}
	if err := validateHistory(*history, next, manifest, roots); err != nil {
		return err
	}
	path := filepath.Join(*history, fmt.Sprintf("g%016d-s%016d.json", payload.Generation, payload.Serial))
	if err := distinctPaths(*keyPath, *certPath, *source, *release, *output, path); err != nil {
		return err
	}
	if err := writeHistory(path, signed); err != nil {
		return err
	}
	if err := writePublic(*output, signed); err != nil {
		return err
	}
	manifest.Snapshot = signed
	return writeJSON(*release, manifest)
}

func validateHistory(directory string, next nodedirectory.Snapshot, manifest releaseManifest, roots []nodedirectory.Root) error {
	entries, err := os.ReadDir(directory)
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	var highest nodedirectory.Cursor
	aliases := map[string]string{}
	accept := func(data []byte) error {
		prior, err := nodedirectory.Verify(data, manifest.Network, roots)
		if err != nil {
			return err
		}
		aliases, err = nodedirectory.RetainAliases(aliases, prior)
		if err != nil {
			return err
		}
		cursor := prior.Cursor()
		if cursor.Generation > highest.Generation || cursor.Generation == highest.Generation && cursor.Serial >= highest.Serial {
			if err := nodedirectory.Compare(highest, cursor); err != nil {
				return err
			}
			highest = cursor
		}
		return nil
	}
	for _, entry := range entries {
		if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".json") {
			continue
		}
		data, err := os.ReadFile(filepath.Join(directory, entry.Name()))
		if err != nil {
			return err
		}
		if err := accept(data); err != nil {
			return err
		}
	}
	if len(manifest.Snapshot) > 0 && string(manifest.Snapshot) != "null" {
		if err := accept(manifest.Snapshot); err != nil {
			return err
		}
	}
	if err := nodedirectory.Accept(highest, next, time.Now(), 0); err != nil {
		return err
	}
	_, err = nodedirectory.RetainAliases(aliases, next)
	return err
}
