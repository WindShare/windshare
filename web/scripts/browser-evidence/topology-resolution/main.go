package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"time"

	"github.com/windshare/windshare/internal/testicetopology"
)

const (
	resolutionTimeout       = 10 * time.Second
	privateDirectoryMode    = 0o700
	privateResolutionMode   = 0o600
	topologyResolutionUsage = "usage: go run ./web/scripts/browser-evidence/topology-resolution " +
		"--profile <absolute canonical profile.json> --output <fresh absolute resolution.json>"
)

type topologyResolver interface {
	Resolve(context.Context, testicetopology.Profile) (testicetopology.Resolution, error)
}

type commandOptions struct {
	profilePath    string
	resolutionPath string
}

type pendingAtomicPublication struct {
	temporaryPath string
	destination   string
}

type materializationRecord struct {
	Component                string `json:"component"`
	Outcome                  string `json:"outcome"`
	ProfilePath              string `json:"profilePath"`
	ResolutionPath           string `json:"resolutionPath"`
	TopologyProfileSHA256    string `json:"topologyProfileSha256"`
	TopologyResolutionSHA256 string `json:"topologyResolutionSha256"`
}

type selfCheckRecord struct {
        SchemaVersion int    `json:"schemaVersion"`
        Component     string `json:"component"`
        Outcome       string `json:"outcome"`
}

func main() {
	err := execute(
		context.Background(),
		os.Args[1:],
		testicetopology.NewStandardResolver(),
		os.Stdout,
	)
	if err == nil {
		return
	}
	_ = json.NewEncoder(os.Stderr).Encode(struct {
		Component string `json:"component"`
		Outcome   string `json:"outcome"`
		Error     string `json:"error"`
	}{
		Component: "browser-evidence-topology-resolution",
		Outcome:   "failed",
		Error:     err.Error(),
	})
	os.Exit(1)
}

func execute(
	ctx context.Context,
	args []string,
	resolver topologyResolver,
	stdout io.Writer,
) error {
	if stdout == nil {
		return errors.New("stdout writer is required")
	}
        if len(args) == 1 && (args[0] == "--help" || args[0] == "-h") {
		_, err := fmt.Fprintln(stdout, topologyResolutionUsage)
		return err
        }
        if len(args) == 1 && args[0] == "self-check" {
                return json.NewEncoder(stdout).Encode(selfCheckRecord{
                        SchemaVersion: 1,
                        Component:     "browser-evidence-topology-resolution",
                        Outcome:       "ready",
                })
        }
	options, err := parseCommandOptions(args)
	if err != nil {
		return err
	}
	if resolver == nil {
		return errors.New("topology resolver is required")
	}
	profile, err := loadCanonicalProfile(options.profilePath)
	if err != nil {
		return fmt.Errorf("load topology profile: %w", err)
	}
	profileSHA256, err := profile.SHA256()
	if err != nil {
		return fmt.Errorf("hash topology profile: %w", err)
	}
	resolutionContext, cancel := context.WithTimeout(ctx, resolutionTimeout)
	defer cancel()
	resolution, err := resolver.Resolve(resolutionContext, profile)
	if err != nil {
		return fmt.Errorf("resolve current-machine topology: %w", err)
	}
	encoded, err := resolution.CanonicalJSON(profile, profileSHA256)
	if err != nil {
		return fmt.Errorf("encode current-machine topology resolution: %w", err)
	}
	resolutionSHA256, err := resolution.SHA256(profile, profileSHA256)
	if err != nil {
		return fmt.Errorf("hash current-machine topology resolution: %w", err)
	}
	// The exit status authorizes this record. Emitting it before staging keeps an
	// untrusted or blocked writer outside the temporary file's lifetime.
	if err := json.NewEncoder(stdout).Encode(materializationRecord{
		Component:                "browser-evidence-topology-resolution",
		Outcome:                  "materialized",
		ProfilePath:              options.profilePath,
		ResolutionPath:           options.resolutionPath,
		TopologyProfileSHA256:    profileSHA256,
		TopologyResolutionSHA256: resolutionSHA256,
	}); err != nil {
		return fmt.Errorf("write topology materialization record: %w", err)
	}
	publication, err := stageNewAtomic(options.resolutionPath, encoded)
	if err != nil {
		return fmt.Errorf("publish current-machine topology resolution: %w", err)
	}
	defer publication.discard()
	// Keeping the no-clobber link adjacent to staging and as the final fallible
	// step prevents a later failure from contradicting successful publication.
	if err := publication.commit(); err != nil {
		return fmt.Errorf("publish current-machine topology resolution: %w", err)
	}
	return nil
}

func loadCanonicalProfile(path string) (testicetopology.Profile, error) {
	profileFile, err := os.Open(path)
	if err != nil {
		return testicetopology.Profile{}, err
	}
	maximumBytes := int64(testicetopology.MaximumFileBytes)
	encoded, readErr := io.ReadAll(io.LimitReader(profileFile, maximumBytes+1))
	closeErr := profileFile.Close()
	if readErr != nil {
		return testicetopology.Profile{}, readErr
	}
	if closeErr != nil {
		return testicetopology.Profile{}, closeErr
	}
	if int64(len(encoded)) > maximumBytes {
		return testicetopology.Profile{}, errors.New("topology profile exceeds the frozen byte limit")
	}
	profile, err := testicetopology.Parse(encoded)
	if err != nil {
		return testicetopology.Profile{}, err
	}
	canonical, err := profile.CanonicalJSON()
	if err != nil {
		return testicetopology.Profile{}, err
	}
	if !bytes.Equal(encoded, canonical) {
		return testicetopology.Profile{}, errors.New("topology profile bytes differ from the exact canonical encoding")
	}
	return profile, nil
}

func parseCommandOptions(args []string) (commandOptions, error) {
	flags := flag.NewFlagSet("topology-resolution", flag.ContinueOnError)
	flags.SetOutput(io.Discard)
	profilePath := flags.String("profile", "", "absolute canonical topology profile path")
	resolutionPath := flags.String("output", "", "absolute canonical resolution output path")
	if err := flags.Parse(args); err != nil {
		return commandOptions{}, fmt.Errorf("parse topology resolution options: %w", err)
	}
	if flags.NArg() != 0 {
		return commandOptions{}, errors.New("topology resolution command does not accept positional arguments")
	}
	profile, err := canonicalAbsolutePath(*profilePath, "profile")
	if err != nil {
		return commandOptions{}, err
	}
	resolution, err := canonicalAbsolutePath(*resolutionPath, "output")
	if err != nil {
		return commandOptions{}, err
	}
	if samePath(profile, resolution) {
		return commandOptions{}, errors.New("topology profile and resolution output paths must differ")
	}
	return commandOptions{profilePath: profile, resolutionPath: resolution}, nil
}

func canonicalAbsolutePath(path, label string) (string, error) {
	if path == "" {
		return "", fmt.Errorf("--%s is required", label)
	}
	if !filepath.IsAbs(path) || filepath.Clean(path) != path {
		return "", fmt.Errorf("--%s must be an absolute canonical path", label)
	}
	return path, nil
}

func samePath(left, right string) bool {
	if runtime.GOOS == "windows" {
		return strings.EqualFold(left, right)
	}
	return left == right
}

func writeNewAtomic(path string, encoded []byte) error {
	publication, err := stageNewAtomic(path, encoded)
	if err != nil {
		return err
	}
	defer publication.discard()
	return publication.commit()
}

func stageNewAtomic(path string, encoded []byte) (*pendingAtomicPublication, error) {
	directory := filepath.Dir(path)
	if err := os.MkdirAll(directory, privateDirectoryMode); err != nil {
		return nil, err
	}
	if _, err := os.Lstat(path); err == nil {
		return nil, errors.New("resolution output already exists; use a fresh run path")
	} else if !errors.Is(err, os.ErrNotExist) {
		return nil, err
	}
	temporary, err := os.CreateTemp(directory, ".topology-resolution-*.tmp")
	if err != nil {
		return nil, err
	}
	publication := &pendingAtomicPublication{
		temporaryPath: temporary.Name(),
		destination:   path,
	}
	staged := false
	defer func() {
		if !staged {
			_ = temporary.Close()
			publication.discard()
		}
	}()
	if err := temporary.Chmod(privateResolutionMode); err != nil {
		return nil, err
	}
	if _, err := temporary.Write(encoded); err != nil {
		return nil, err
	}
	if err := temporary.Sync(); err != nil {
		return nil, err
	}
	if err := temporary.Close(); err != nil {
		return nil, err
	}
	staged = true
	return publication, nil
}

func (publication *pendingAtomicPublication) commit() error {
	// Linking a flushed same-directory temporary file publishes the name with
	// O_EXCL semantics. Unlike POSIX rename, it cannot overwrite a destination
	// created between the earlier diagnostic check and this commit point.
	return os.Link(publication.temporaryPath, publication.destination)
}

func (publication *pendingAtomicPublication) discard() {
	_ = os.Remove(publication.temporaryPath)
}
