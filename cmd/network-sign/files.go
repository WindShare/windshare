package main

import (
	"bytes"
	"crypto/ed25519"
	"crypto/x509"
	"encoding/json"
	"encoding/pem"
	"errors"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"strings"

	"github.com/windshare/windshare/core/nodedirectory"
)

func readJSON(path string, target any) error {
	file, err := os.Open(path)
	if err != nil {
		return err
	}
	defer file.Close()
	data, err := io.ReadAll(io.LimitReader(file, nodedirectory.MaximumEnvelopeBytes+1))
	if err != nil {
		return err
	}
	if len(data) > nodedirectory.MaximumEnvelopeBytes {
		return nodedirectory.ErrFormat
	}
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(target); err != nil {
		return err
	}
	if decoder.Decode(new(any)) != io.EOF {
		return nodedirectory.ErrFormat
	}
	return nil
}

func distinctPaths(paths ...string) error {
	resolved := make([]string, len(paths))
	for i, path := range paths {
		if path == "" {
			return errors.New("input and output paths are required")
		}
		absolute, err := filepath.Abs(path)
		if err != nil {
			return err
		}
		if target, err := filepath.EvalSymlinks(absolute); err == nil {
			absolute = target
		}
		if runtime.GOOS == "windows" {
			absolute = strings.ToLower(absolute)
		}
		for j := range i {
			left, leftErr := os.Stat(path)
			right, rightErr := os.Stat(paths[j])
			if absolute == resolved[j] || leftErr == nil && rightErr == nil && os.SameFile(left, right) {
				return errors.New("signing inputs and outputs must be distinct files")
			}
		}
		resolved[i] = absolute
	}
	return nil
}

func privateKey(path string) (ed25519.PrivateKey, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	defer clear(data)
	block, rest := pem.Decode(data)
	if block == nil || block.Type != "PRIVATE KEY" || len(bytes.TrimSpace(rest)) != 0 {
		return nil, errors.New("expected one PKCS8 private key")
	}
	defer clear(block.Bytes)
	key, err := x509.ParsePKCS8PrivateKey(block.Bytes)
	if err != nil {
		return nil, err
	}
	ed, ok := key.(ed25519.PrivateKey)
	if !ok {
		return nil, errors.New("expected Ed25519 private key")
	}
	return ed, nil
}

func writeJSON(path string, value any) error {
	data, err := json.MarshalIndent(value, "", "  ")
	if err != nil {
		return err
	}
	return writePublic(path, append(data, '\n'))
}

func writePublic(path string, data []byte) error {
	if path == "" {
		return errors.New("output path is required")
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	file, err := os.CreateTemp(filepath.Dir(path), ".network-sign-*")
	if err != nil {
		return err
	}
	defer func() { _ = os.Remove(file.Name()) }()
	if _, err = file.Write(data); err != nil {
		_ = file.Close()
		return err
	}
	if err = file.Sync(); err != nil {
		_ = file.Close()
		return err
	}
	if err = file.Close(); err != nil {
		return err
	}
	if err = os.Chmod(file.Name(), 0o644); err != nil {
		return err
	}
	return os.Rename(file.Name(), path)
}
