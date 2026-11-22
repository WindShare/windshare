package main

import (
	"crypto/sha256"
	"errors"
	"flag"
	"fmt"
	"os"
	"strings"

	"golang.org/x/mod/module"
)

// _sourcebundle builds and extracts the complete supported source bundle.
// Nested pinned dependency modules are required build inputs, never omitted.
// Reading commit blobs excludes worktree, index, and workspace state.

const modulePath = "github.com/windshare/windshare"

var requiredFiles = []string{
	".testcoverage.yml",
	"go.mod",
	"go.sum",
	"LICENSE",
	"NOTICE",
	"cmd/wind/main.go",
	"core/testvectors/README.md",
	"core/testvectors/inventory.txt",
}

type configuration struct {
	repositoryRoot string
	commitSHA      string
	zipPath        string
	extractPath    string
	version        string
}

type committedFile struct {
	relativePath string
	objectID     string
	executable   bool
}

type builtBundle struct {
	zipDigest   [sha256.Size]byte
	fileDigests map[string][sha256.Size]byte
}

func main() {
	config := configuration{}
	flag.StringVar(&config.repositoryRoot, "repo", "", "repository root")
	flag.StringVar(&config.commitSHA, "commit", "", "exact lowercase 40-character release commit SHA")
	flag.StringVar(&config.zipPath, "zip", "", "source bundle output path")
	flag.StringVar(&config.extractPath, "extract", "", "empty directory for the extracted module")
	flag.StringVar(&config.version, "version", "", "root module semantic version")
	flag.Parse()

	if err := run(config); err != nil {
		fmt.Fprintf(os.Stderr, "release artifact: %v\n", err)
		os.Exit(1)
	}
}

func run(config configuration) (runErr error) {
	if err := validateConfiguration(config); err != nil {
		return err
	}

	files, err := committedModuleFiles(config.repositoryRoot, config.commitSHA)
	if err != nil {
		return err
	}
	if len(files) == 0 {
		return errors.New("release commit contains no module files")
	}
	if err := auditCommittedProjection(files); err != nil {
		return err
	}

	version := module.Version{Path: modulePath, Version: config.version}
	bundle, err := buildSourceBundle(config.repositoryRoot, config.zipPath, version, files)
	if err != nil {
		return err
	}
	archiveValidated := false
	defer func() {
		if !archiveValidated {
			runErr = removeFailedArchive(config.zipPath, runErr)
		}
	}()

	if err := extractSourceBundle(
		config.zipPath,
		config.extractPath,
		version,
		committedFilePaths(files),
		bundle.fileDigests,
	); err != nil {
		return err
	}
	archiveValidated = true

	fmt.Printf("source=%s@%s\n", modulePath, config.version)
	fmt.Printf("commit=%s\n", config.commitSHA)
	fmt.Printf("files=%d\n", len(files))
	fmt.Printf("sha256=%x\n", bundle.zipDigest)
	return nil
}

func validateConfiguration(config configuration) error {
	for name, value := range map[string]string{
		"repo":    config.repositoryRoot,
		"commit":  config.commitSHA,
		"zip":     config.zipPath,
		"extract": config.extractPath,
		"version": config.version,
	} {
		if strings.TrimSpace(value) == "" {
			return fmt.Errorf("-%s is required", name)
		}
	}
	if err := module.Check(modulePath, config.version); err != nil {
		return fmt.Errorf("invalid module version: %w", err)
	}
	if !isLowerHexCommitSHA(config.commitSHA) {
		return errors.New("-commit must be an exact lowercase 40-character commit SHA")
	}
	return nil
}

func isLowerHexCommitSHA(value string) bool {
	if len(value) != 40 {
		return false
	}
	for _, character := range value {
		if (character < '0' || character > '9') && (character < 'a' || character > 'f') {
			return false
		}
	}
	return true
}
