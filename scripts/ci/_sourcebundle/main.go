package main

import (
	"errors"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"golang.org/x/mod/module"
)

// release-archive builds and extracts the complete supported source bundle.
// Nested pinned dependency modules are required build inputs, never omitted.
// Reading commit blobs excludes worktree and caller-controlled workspace state.

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
	stageDirectory string
	zipPath        string
	extractPath    string
	version        string
}

type committedFile struct {
	relativePath string
	objectID     string
	executable   bool
}

func main() {
	config := configuration{}
	flag.StringVar(&config.repositoryRoot, "repo", "", "repository root")
	flag.StringVar(&config.commitSHA, "commit", "", "exact lowercase 40-character release commit SHA")
	flag.StringVar(&config.stageDirectory, "stage", "", "empty directory for committed module sources")
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

	if err := os.MkdirAll(config.stageDirectory, 0o755); err != nil {
		return fmt.Errorf("create staging directory: %w", err)
	}
	if err := requireEmptyDirectory(config.stageDirectory); err != nil {
		return err
	}
	if err := stageFiles(config.repositoryRoot, config.stageDirectory, files); err != nil {
		return err
	}
	if err := validateReleaseMetadata(config.stageDirectory); err != nil {
		return err
	}
	projectedPaths, err := validateSourceInput(config.stageDirectory)
	if err != nil {
		return err
	}

	version := module.Version{Path: modulePath, Version: config.version}
	if err := createSourceBundle(config.zipPath, version, config.stageDirectory, files); err != nil {
		return err
	}
	archiveValidated := false
	defer func() {
		if !archiveValidated {
			runErr = removeFailedArchive(config.zipPath, runErr)
		}
	}()

	// A second construction proves byte determinism instead of trusting file
	// timestamps or platform-specific archive defaults.
	secondZip := config.zipPath + ".determinism-check"
	if err := createSourceBundle(secondZip, version, config.stageDirectory, files); err != nil {
		return err
	}
	defer func() {
		if err := os.Remove(secondZip); err != nil && !errors.Is(err, os.ErrNotExist) {
			// A leaked verifier makes the release workspace ambiguous, so its
			// cleanup failure also invalidates publication of the primary zip.
			archiveValidated = false
			runErr = errors.Join(runErr, fmt.Errorf("remove determinism-check source bundle: %w", err))
		}
	}()
	firstDigest, err := fileDigest(config.zipPath)
	if err != nil {
		return err
	}
	secondDigest, err := fileDigest(secondZip)
	if err != nil {
		return err
	}
	if firstDigest != secondDigest {
		return errors.New("source bundle is not byte-deterministic")
	}

	if err := extractSourceBundle(config.zipPath, config.extractPath, version, projectedPaths); err != nil {
		return err
	}
	for _, relativePath := range projectedPaths {
		staged, err := fileDigest(filepath.Join(config.stageDirectory, filepath.FromSlash(relativePath)))
		if err != nil {
			return err
		}
		extracted, err := fileDigest(filepath.Join(config.extractPath, filepath.FromSlash(relativePath)))
		if err != nil {
			return err
		}
		if staged != extracted {
			return fmt.Errorf("extracted source differs: %s", relativePath)
		}
	}
	archiveValidated = true

	fmt.Printf("source=%s@%s\n", modulePath, config.version)
	fmt.Printf("commit=%s\n", config.commitSHA)
	fmt.Printf("files=%d\n", len(files))
	fmt.Printf("sha256=%x\n", firstDigest)
	return nil
}

func validateConfiguration(config configuration) error {
	for name, value := range map[string]string{
		"repo":    config.repositoryRoot,
		"commit":  config.commitSHA,
		"stage":   config.stageDirectory,
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
