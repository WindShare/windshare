// core-release-archive builds the exact source artifact that a core/* tag
// publishes, then extracts it so validation cannot accidentally resolve files
// through the repository's parent module or go.work.
package main

import (
	"bufio"
	"bytes"
	"crypto/sha256"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"sort"
	"strings"

	"golang.org/x/mod/modfile"
	"golang.org/x/mod/module"
	modzip "golang.org/x/mod/zip"
)

const (
	modulePath          = "github.com/windshare/windshare/core"
	closedModuleVersion = "v0.3.0"
)

var allowedTopLevelFiles = map[string]struct{}{
	".testcoverage.yml": {},
	"LICENSE":           {},
	"NOTICE":            {},
	"README.md":         {},
	"go.mod":            {},
	"go.sum":            {},
}

var allowedTopLevelDirectories = map[string]struct{}{
	"catalog":      {},
	"content":      {},
	"framechannel": {},
	"internal":     {},
	"link":         {},
	"liveshare":    {},
	"osfs":         {},
	"senderobject": {},
	"session":      {},
	"testvectors":  {},
	"transfer":     {},
}

var requiredFiles = []string{
	".testcoverage.yml",
	"go.mod",
	"go.sum",
	"README.md",
	"LICENSE",
	"NOTICE",
	"testvectors/README.md",
	"testvectors/inventory.txt",
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
}

func main() {
	config := configuration{}
	flag.StringVar(&config.repositoryRoot, "repo", "", "repository root")
	flag.StringVar(&config.commitSHA, "commit", "", "exact lowercase 40-character release commit SHA")
	flag.StringVar(&config.stageDirectory, "stage", "", "empty directory for committed core sources")
	flag.StringVar(&config.zipPath, "zip", "", "module zip output path")
	flag.StringVar(&config.extractPath, "extract", "", "empty directory for the extracted module")
	flag.StringVar(&config.version, "version", "", "core semantic version")
	flag.Parse()

	if err := run(config); err != nil {
		fmt.Fprintf(os.Stderr, "core release artifact: %v\n", err)
		os.Exit(1)
	}
}

func run(config configuration) (runErr error) {
	if err := validateConfiguration(config); err != nil {
		return err
	}

	files, err := committedCoreFiles(config.repositoryRoot, config.commitSHA)
	if err != nil {
		return err
	}
	if len(files) == 0 {
		return errors.New("release commit contains no core files")
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
	projectedPaths := committedFilePaths(files)
	if err := validateModuleZipInput(config.stageDirectory, projectedPaths); err != nil {
		return err
	}

	version := module.Version{Path: modulePath, Version: config.version}
	if err := createModuleZip(config.zipPath, version, config.stageDirectory); err != nil {
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
	if err := createModuleZip(secondZip, version, config.stageDirectory); err != nil {
		return err
	}
	defer func() {
		if err := os.Remove(secondZip); err != nil && !errors.Is(err, os.ErrNotExist) {
			// A leaked verifier makes the release workspace ambiguous, so its
			// cleanup failure also invalidates publication of the primary zip.
			archiveValidated = false
			runErr = errors.Join(runErr, fmt.Errorf("remove determinism-check module zip: %w", err))
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
		return errors.New("canonical module zip is not byte-deterministic")
	}

	checked, err := modzip.CheckZip(version, config.zipPath)
	if err != nil {
		return fmt.Errorf("check module zip: %w", err)
	}
	if err := validateModuleZipProjection(version, checked.Valid, projectedPaths); err != nil {
		return err
	}

	if err := modzip.Unzip(config.extractPath, version, config.zipPath); err != nil {
		return fmt.Errorf("extract module zip: %w", err)
	}
	archiveValidated = true

	fmt.Printf("module=%s@%s\n", modulePath, config.version)
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
	// Keep the canceled namespace unreachable even when a caller bypasses the
	// workflow resolver and invokes an artifact script directly.
	if config.version == closedModuleVersion {
		return fmt.Errorf("module version %s is closed and cannot be validated again", config.version)
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

func committedCoreFiles(repositoryRoot, commitSHA string) ([]committedFile, error) {
	objectType, err := gitOutput(repositoryRoot, "cat-file", "-t", commitSHA)
	if err != nil {
		return nil, fmt.Errorf("inspect release commit: %w", err)
	}
	if strings.TrimSpace(string(objectType)) != "commit" {
		return nil, errors.New("release SHA must directly identify a commit object")
	}

	output, err := gitOutput(
		repositoryRoot,
		"ls-tree", "-r", "-z", "--full-tree", commitSHA, "--", "core",
	)
	if err != nil {
		return nil, fmt.Errorf("enumerate committed core files: %w", err)
	}
	return parseCommittedCoreFiles(output)
}

func gitOutput(repositoryRoot string, arguments ...string) ([]byte, error) {
	command := exec.Command("git", append([]string{"-C", repositoryRoot}, arguments...)...)
	command.Env = isolatedGitEnvironment()
	output, err := command.Output()
	if err == nil {
		return output, nil
	}
	var exitError *exec.ExitError
	if errors.As(err, &exitError) {
		return nil, fmt.Errorf(
			"git %s: %s",
			strings.Join(arguments, " "),
			strings.TrimSpace(string(exitError.Stderr)),
		)
	}
	return nil, fmt.Errorf("git %s: %w", strings.Join(arguments, " "), err)
}

func isolatedGitEnvironment() []string {
	environment := make([]string, 0, len(os.Environ())+4)
	for _, variable := range os.Environ() {
		key, _, _ := strings.Cut(variable, "=")
		if strings.HasPrefix(strings.ToUpper(key), "GIT_") {
			continue
		}
		environment = append(environment, variable)
	}
	// The publication set must come from the named repository, not caller
	// redirects or machine-global ignore configuration.
	return append(
		environment,
		"GIT_CONFIG_NOSYSTEM=1",
		"GIT_CONFIG_GLOBAL="+os.DevNull,
		"GIT_NO_REPLACE_OBJECTS=1",
		"GIT_TERMINAL_PROMPT=0",
	)
}

func parseCommittedCoreFiles(output []byte) ([]committedFile, error) {
	seen := make(map[string]struct{})
	files := make([]committedFile, 0)
	for _, record := range bytes.Split(output, []byte{0}) {
		if len(record) == 0 {
			continue
		}
		header, gitPathBytes, found := bytes.Cut(record, []byte{'\t'})
		if !found {
			return nil, fmt.Errorf("Git returned a malformed tree record: %q", record)
		}
		fields := strings.Fields(string(header))
		if len(fields) != 3 {
			return nil, fmt.Errorf("Git returned a malformed tree header: %q", header)
		}
		mode, objectType, objectID := fields[0], fields[1], fields[2]
		if objectType != "blob" || (mode != "100644" && mode != "100755") {
			return nil, fmt.Errorf("committed core path is not a regular file: %s", gitPathBytes)
		}
		if !isLowerHexCommitSHA(objectID) {
			return nil, fmt.Errorf("committed core path has an invalid object ID: %s", gitPathBytes)
		}

		fullGitPath := string(gitPathBytes)
		if strings.Contains(fullGitPath, "\\") {
			return nil, fmt.Errorf("Git returned a non-canonical path: %q", fullGitPath)
		}
		const prefix = "core/"
		if !strings.HasPrefix(fullGitPath, prefix) {
			return nil, fmt.Errorf("Git returned a path outside core: %q", fullGitPath)
		}
		relativePath := strings.TrimPrefix(fullGitPath, prefix)
		if !isSafeModulePath(relativePath) {
			return nil, fmt.Errorf("invalid core module path: %q", relativePath)
		}
		if err := validateTopLevelPath(relativePath); err != nil {
			return nil, err
		}
		if _, duplicate := seen[relativePath]; duplicate {
			return nil, fmt.Errorf("duplicate core module path: %s", relativePath)
		}
		seen[relativePath] = struct{}{}
		files = append(files, committedFile{relativePath: relativePath, objectID: objectID})
	}
	sort.Slice(files, func(left, right int) bool {
		return files[left].relativePath < files[right].relativePath
	})
	return files, nil
}

func committedFilePaths(files []committedFile) []string {
	paths := make([]string, len(files))
	for index, file := range files {
		paths[index] = file.relativePath
	}
	return paths
}

func isSafeModulePath(filePath string) bool {
	if filePath == "" || filePath == "." || path.IsAbs(filePath) || strings.Contains(filePath, "\\") {
		return false
	}
	cleaned := path.Clean(filePath)
	if cleaned != filePath || cleaned == ".." || strings.HasPrefix(cleaned, "../") {
		return false
	}
	return module.CheckFilePath(filePath) == nil
}

func validateTopLevelPath(filePath string) error {
	topLevel, remainder, nested := strings.Cut(filePath, "/")
	if !nested {
		if _, allowed := allowedTopLevelFiles[topLevel]; !allowed {
			return fmt.Errorf("unexpected top-level core file: %s", filePath)
		}
		return nil
	}
	if remainder == "" {
		return fmt.Errorf("invalid empty path below core/%s", topLevel)
	}
	if _, allowed := allowedTopLevelDirectories[topLevel]; !allowed {
		return fmt.Errorf("unexpected top-level core directory: %s", topLevel)
	}
	return nil
}

func auditCommittedProjection(files []committedFile) error {
	seenFiles := make(map[string]struct{}, len(allowedTopLevelFiles))
	seenDirectories := make(map[string]struct{}, len(allowedTopLevelDirectories))
	for _, file := range files {
		topLevel, _, nested := strings.Cut(file.relativePath, "/")
		if nested {
			seenDirectories[topLevel] = struct{}{}
		} else {
			seenFiles[topLevel] = struct{}{}
		}
	}
	if missing := setDifference(allowedTopLevelFiles, seenFiles); len(missing) != 0 {
		return fmt.Errorf("required top-level core files are missing: %s", strings.Join(missing, ", "))
	}
	if missing := setDifference(allowedTopLevelDirectories, seenDirectories); len(missing) != 0 {
		return fmt.Errorf("required top-level core directories are missing: %s", strings.Join(missing, ", "))
	}
	return nil
}

func requireEmptyDirectory(directory string) error {
	entries, err := os.ReadDir(directory)
	if err != nil {
		return fmt.Errorf("read staging directory: %w", err)
	}
	if len(entries) != 0 {
		return fmt.Errorf("staging directory is not empty: %s", directory)
	}
	return nil
}

func stageFiles(repositoryRoot, stageDirectory string, files []committedFile) error {
	for _, file := range files {
		relativePath := file.relativePath
		if !isSafeModulePath(relativePath) {
			return fmt.Errorf("invalid staged module path: %q", relativePath)
		}
		if err := validateTopLevelPath(relativePath); err != nil {
			return err
		}
		destinationPath := filepath.Join(stageDirectory, filepath.FromSlash(relativePath))
		if err := os.MkdirAll(filepath.Dir(destinationPath), 0o755); err != nil {
			return fmt.Errorf("create staging parent for %s: %w", relativePath, err)
		}
		if err := copyCommittedBlob(repositoryRoot, file.objectID, destinationPath); err != nil {
			return err
		}
	}
	return nil
}

func copyCommittedBlob(repositoryRoot, objectID, destinationPath string) error {
	destination, err := os.OpenFile(destinationPath, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o644)
	if err != nil {
		return fmt.Errorf("create %s: %w", destinationPath, err)
	}
	command := exec.Command("git", "-C", repositoryRoot, "cat-file", "blob", objectID)
	command.Env = isolatedGitEnvironment()
	command.Stdout = destination
	var stderr bytes.Buffer
	command.Stderr = &stderr
	copyErr := command.Run()
	closeErr := destination.Close()
	if copyErr != nil {
		removeErr := os.Remove(destinationPath)
		return errors.Join(
			fmt.Errorf("read committed blob %s: %s: %w", objectID, strings.TrimSpace(stderr.String()), copyErr),
			removeErr,
		)
	}
	if closeErr != nil {
		return fmt.Errorf("close %s: %w", destinationPath, closeErr)
	}
	return nil
}

func validateReleaseMetadata(stageDirectory string) error {
	for _, relativePath := range requiredFiles {
		info, err := os.Stat(filepath.Join(stageDirectory, filepath.FromSlash(relativePath)))
		if err != nil {
			return fmt.Errorf("required release file %s: %w", relativePath, err)
		}
		if !info.Mode().IsRegular() || info.Size() == 0 {
			return fmt.Errorf("required release file is empty or irregular: %s", relativePath)
		}
	}

	goMod, err := os.ReadFile(filepath.Join(stageDirectory, "go.mod"))
	if err != nil {
		return fmt.Errorf("read go.mod: %w", err)
	}
	if actual := modfile.ModulePath(goMod); actual != modulePath {
		return fmt.Errorf("go.mod module path is %q, want %q", actual, modulePath)
	}
	if err := requireText(filepath.Join(stageDirectory, "LICENSE"), "Apache License", "Version 2.0"); err != nil {
		return err
	}
	if err := requireText(filepath.Join(stageDirectory, "NOTICE"), "WindShare"); err != nil {
		return err
	}
	if err := validateVectorInventory(stageDirectory); err != nil {
		return err
	}
	return nil
}

func requireText(filePath string, required ...string) error {
	content, err := os.ReadFile(filePath)
	if err != nil {
		return fmt.Errorf("read %s: %w", filePath, err)
	}
	for _, text := range required {
		if !bytes.Contains(content, []byte(text)) {
			return fmt.Errorf("%s does not contain required text %q", filePath, text)
		}
	}
	return nil
}

func validateVectorInventory(stageDirectory string) error {
	vectorDirectory := filepath.Join(stageDirectory, "testvectors")
	inventoryPath := filepath.Join(vectorDirectory, "inventory.txt")
	inventory, err := os.Open(inventoryPath)
	if err != nil {
		return fmt.Errorf("open testvector inventory: %w", err)
	}
	defer inventory.Close()

	expected := make(map[string]struct{})
	scanner := bufio.NewScanner(inventory)
	for scanner.Scan() {
		entry := strings.TrimSpace(scanner.Text())
		if entry == "" || strings.HasPrefix(entry, "#") {
			continue
		}
		if path.Base(entry) != entry || path.Ext(entry) != ".json" || !isSafeModulePath(entry) {
			return fmt.Errorf("invalid testvector inventory entry: %q", entry)
		}
		if _, duplicate := expected[entry]; duplicate {
			return fmt.Errorf("duplicate testvector inventory entry: %s", entry)
		}
		expected[entry] = struct{}{}
	}
	if err := scanner.Err(); err != nil {
		return fmt.Errorf("read testvector inventory: %w", err)
	}
	if len(expected) == 0 {
		return errors.New("testvector inventory is empty")
	}

	actual := make(map[string]struct{})
	err = filepath.WalkDir(vectorDirectory, func(filePath string, entry os.DirEntry, walkErr error) error {
		if walkErr != nil {
			return fmt.Errorf("inspect testvector path %s: %w", filePath, walkErr)
		}
		if filePath == vectorDirectory {
			return nil
		}
		relativePath, err := filepath.Rel(vectorDirectory, filePath)
		if err != nil {
			return fmt.Errorf("relativize testvector path %s: %w", filePath, err)
		}
		normalized := filepath.ToSlash(relativePath)
		if path.Ext(normalized) != ".json" {
			return nil
		}
		if entry.IsDir() {
			return fmt.Errorf("testvector JSON path is irregular: %s", normalized)
		}
		info, err := entry.Info()
		if err != nil {
			return fmt.Errorf("inspect testvector %s: %w", normalized, err)
		}
		if !info.Mode().IsRegular() {
			return fmt.Errorf("testvector JSON path is irregular: %s", normalized)
		}
		actual[normalized] = struct{}{}
		return nil
	})
	if err != nil {
		return fmt.Errorf("read testvector directory: %w", err)
	}
	if difference := setDifference(expected, actual); len(difference) != 0 {
		return fmt.Errorf("testvector inventory names missing files: %s", strings.Join(difference, ", "))
	}
	if difference := setDifference(actual, expected); len(difference) != 0 {
		return fmt.Errorf("testvector JSON files missing inventory entries: %s", strings.Join(difference, ", "))
	}
	return nil
}

func setDifference(left, right map[string]struct{}) []string {
	difference := make([]string, 0)
	for item := range left {
		if _, found := right[item]; !found {
			difference = append(difference, item)
		}
	}
	sort.Strings(difference)
	return difference
}

func validateModuleZipInput(stageDirectory string, committedFiles []string) error {
	checked, err := modzip.CheckDir(stageDirectory)
	if err != nil {
		return fmt.Errorf("check staged module: %w", err)
	}
	if len(checked.Omitted) != 0 {
		return fmt.Errorf("module zip would omit projected files: %v", checked.Omitted)
	}
	if len(checked.Invalid) != 0 || checked.SizeError != nil {
		return fmt.Errorf("module zip contains invalid files: %v; size: %v", checked.Invalid, checked.SizeError)
	}
	accepted := make([]string, 0, len(checked.Valid))
	for _, filePath := range checked.Valid {
		relativePath, err := filepath.Rel(stageDirectory, filePath)
		if err != nil {
			return fmt.Errorf("relativize staged module path %s: %w", filePath, err)
		}
		accepted = append(accepted, filepath.ToSlash(relativePath))
	}
	return validateExactProjection(accepted, committedFiles, "staged module")
}

func validateModuleZipProjection(version module.Version, archiveFiles, committedFiles []string) error {
	prefix := version.Path + "@" + version.Version + "/"
	normalized := make([]string, 0, len(archiveFiles))
	for _, archivePath := range archiveFiles {
		if !strings.HasPrefix(archivePath, prefix) {
			return fmt.Errorf("module zip path lacks canonical prefix %q: %s", prefix, archivePath)
		}
		normalized = append(normalized, strings.TrimPrefix(archivePath, prefix))
	}
	return validateExactProjection(normalized, committedFiles, "module zip")
}

func validateExactProjection(actualFiles, committedFiles []string, actualName string) error {
	actual, err := modulePathSet(actualFiles, actualName)
	if err != nil {
		return err
	}
	committed, err := modulePathSet(committedFiles, "committed projection")
	if err != nil {
		return err
	}
	if difference := setDifference(actual, committed); len(difference) != 0 {
		return fmt.Errorf("%s has files outside the committed projection: %s", actualName, strings.Join(difference, ", "))
	}
	if difference := setDifference(committed, actual); len(difference) != 0 {
		return fmt.Errorf("committed projection has files absent from the %s: %s", actualName, strings.Join(difference, ", "))
	}
	return nil
}

func modulePathSet(files []string, owner string) (map[string]struct{}, error) {
	result := make(map[string]struct{}, len(files))
	for _, filePath := range files {
		if !isSafeModulePath(filePath) {
			return nil, fmt.Errorf("%s contains invalid module path: %q", owner, filePath)
		}
		if err := validateTopLevelPath(filePath); err != nil {
			return nil, err
		}
		if _, duplicate := result[filePath]; duplicate {
			return nil, fmt.Errorf("%s contains duplicate module path: %s", owner, filePath)
		}
		result[filePath] = struct{}{}
	}
	return result, nil
}

func createModuleZip(zipPath string, version module.Version, stageDirectory string) error {
	if err := os.MkdirAll(filepath.Dir(zipPath), 0o755); err != nil {
		return fmt.Errorf("create module zip parent: %w", err)
	}
	output, err := os.OpenFile(zipPath, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o644)
	if err != nil {
		return fmt.Errorf("create module zip: %w", err)
	}
	createErr := modzip.CreateFromDir(output, version, stageDirectory)
	closeErr := output.Close()
	if createErr != nil {
		return removeFailedArchive(zipPath, fmt.Errorf("construct module zip: %w", createErr))
	}
	if closeErr != nil {
		return removeFailedArchive(zipPath, fmt.Errorf("close module zip: %w", closeErr))
	}
	return nil
}

func removeFailedArchive(zipPath string, failure error) error {
	if err := os.Remove(zipPath); err != nil && !errors.Is(err, os.ErrNotExist) {
		return errors.Join(failure, fmt.Errorf("remove failed module zip: %w", err))
	}
	return failure
}

func fileDigest(filePath string) ([sha256.Size]byte, error) {
	file, err := os.Open(filePath)
	if err != nil {
		return [sha256.Size]byte{}, fmt.Errorf("open archive for hashing: %w", err)
	}
	defer file.Close()

	digest := sha256.New()
	if _, err := io.Copy(digest, file); err != nil {
		return [sha256.Size]byte{}, fmt.Errorf("hash archive: %w", err)
	}
	var result [sha256.Size]byte
	copy(result[:], digest.Sum(nil))
	return result, nil
}
