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

const modulePath = "github.com/windshare/windshare/core"

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
	stageDirectory string
	zipPath        string
	extractPath    string
	version        string
}

func main() {
	config := configuration{}
	flag.StringVar(&config.repositoryRoot, "repo", "", "repository root")
	flag.StringVar(&config.stageDirectory, "stage", "", "empty directory for projected core sources")
	flag.StringVar(&config.zipPath, "zip", "", "module zip output path")
	flag.StringVar(&config.extractPath, "extract", "", "empty directory for the extracted module")
	flag.StringVar(&config.version, "version", "", "core semantic version")
	flag.Parse()

	if err := run(config); err != nil {
		fmt.Fprintf(os.Stderr, "core release artifact: %v\n", err)
		os.Exit(1)
	}
}

func run(config configuration) error {
	if err := validateConfiguration(config); err != nil {
		return err
	}

	files, err := projectedCoreFiles(config.repositoryRoot)
	if err != nil {
		return err
	}
	if len(files) == 0 {
		return errors.New("Git projection contains no core files")
	}
	coreDirectory := filepath.Join(config.repositoryRoot, "core")
	if err := auditSourceDirectory(coreDirectory, files); err != nil {
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
	if err := validateModuleZipInput(config.stageDirectory, files); err != nil {
		return err
	}

	version := module.Version{Path: modulePath, Version: config.version}
	if err := createModuleZip(config.zipPath, version, config.stageDirectory); err != nil {
		return err
	}

	// A second construction proves byte determinism instead of trusting file
	// timestamps or platform-specific archive defaults.
	secondZip := config.zipPath + ".determinism-check"
	defer os.Remove(secondZip)
	if err := createModuleZip(secondZip, version, config.stageDirectory); err != nil {
		return err
	}
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
	if len(checked.Valid) != len(files) {
		return fmt.Errorf("module zip contains %d files; projected source contains %d", len(checked.Valid), len(files))
	}

	if err := modzip.Unzip(config.extractPath, version, config.zipPath); err != nil {
		return fmt.Errorf("extract module zip: %w", err)
	}

	fmt.Printf("module=%s@%s\n", modulePath, config.version)
	fmt.Printf("files=%d\n", len(files))
	fmt.Printf("sha256=%x\n", firstDigest)
	return nil
}

func validateConfiguration(config configuration) error {
	for name, value := range map[string]string{
		"repo":    config.repositoryRoot,
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
	return nil
}

func projectedCoreFiles(repositoryRoot string) ([]string, error) {
	command := exec.Command(
		"git", "-C", repositoryRoot, "ls-files", "-z",
		"--cached", "--others", "--exclude-standard", "--", "core",
	)
	output, err := command.Output()
	if err != nil {
		var exitError *exec.ExitError
		if errors.As(err, &exitError) {
			return nil, fmt.Errorf("enumerate publishable core files: %s", strings.TrimSpace(string(exitError.Stderr)))
		}
		return nil, fmt.Errorf("enumerate publishable core files: %w", err)
	}

	seen := make(map[string]struct{})
	files := make([]string, 0)
	for _, gitPath := range bytes.Split(output, []byte{0}) {
		if len(gitPath) == 0 {
			continue
		}
		fullGitPath := filepath.ToSlash(string(gitPath))
		const prefix = "core/"
		if !strings.HasPrefix(fullGitPath, prefix) {
			return nil, fmt.Errorf("Git returned a path outside core: %q", fullGitPath)
		}
		relativePath := strings.TrimPrefix(fullGitPath, prefix)
		if !isSafeModulePath(relativePath) {
			return nil, fmt.Errorf("invalid core module path: %q", relativePath)
		}
		sourcePath := filepath.Join(repositoryRoot, filepath.FromSlash(fullGitPath))
		info, err := os.Lstat(sourcePath)
		if errors.Is(err, os.ErrNotExist) {
			// A tracked deletion is absent from the prospective worktree artifact.
			continue
		}
		if err != nil {
			return nil, fmt.Errorf("inspect %s: %w", fullGitPath, err)
		}
		if !info.Mode().IsRegular() {
			return nil, fmt.Errorf("publishable core path is not a regular file: %s", fullGitPath)
		}
		if err := validateTopLevelPath(relativePath); err != nil {
			return nil, err
		}
		if _, duplicate := seen[relativePath]; duplicate {
			return nil, fmt.Errorf("duplicate core module path: %s", relativePath)
		}
		seen[relativePath] = struct{}{}
		files = append(files, relativePath)
	}
	sort.Strings(files)
	return files, nil
}

func isSafeModulePath(filePath string) bool {
	if filePath == "" || filePath == "." || strings.Contains(filePath, "\\") {
		return false
	}
	cleaned := path.Clean(filePath)
	return cleaned == filePath && cleaned != ".." && !strings.HasPrefix(cleaned, "../")
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

func auditSourceDirectory(coreDirectory string, projectedFiles []string) error {
	entries, err := os.ReadDir(coreDirectory)
	if err != nil {
		return fmt.Errorf("read core source directory: %w", err)
	}
	seenFiles := make(map[string]struct{}, len(allowedTopLevelFiles))
	seenDirectories := make(map[string]struct{}, len(allowedTopLevelDirectories))
	for _, entry := range entries {
		name := entry.Name()
		if entry.IsDir() {
			if _, allowed := allowedTopLevelDirectories[name]; !allowed {
				return fmt.Errorf("unexpected top-level core directory: %s", name)
			}
			seenDirectories[name] = struct{}{}
			continue
		}
		info, err := entry.Info()
		if err != nil {
			return fmt.Errorf("inspect core/%s: %w", name, err)
		}
		if !info.Mode().IsRegular() {
			return fmt.Errorf("unexpected irregular top-level core path: %s", name)
		}
		if _, allowed := allowedTopLevelFiles[name]; !allowed {
			return fmt.Errorf("unexpected top-level core file: %s", name)
		}
		seenFiles[name] = struct{}{}
	}
	if missing := setDifference(allowedTopLevelFiles, seenFiles); len(missing) != 0 {
		return fmt.Errorf("required top-level core files are missing: %s", strings.Join(missing, ", "))
	}
	if missing := setDifference(allowedTopLevelDirectories, seenDirectories); len(missing) != 0 {
		return fmt.Errorf("required top-level core directories are missing: %s", strings.Join(missing, ", "))
	}

	// Comparing x/mod's direct source view with Git's prospective publication
	// catches ignored coverage profiles and other local files that CreateFromDir
	// would otherwise silently add to a release built from the working tree.
	checked, err := modzip.CheckDir(coreDirectory)
	if err != nil {
		return fmt.Errorf("check core source directory as module zip input: %w", err)
	}
	if len(checked.Omitted) != 0 {
		return fmt.Errorf("core source contains files omitted by module zip rules: %v", checked.Omitted)
	}

	projected := make(map[string]struct{}, len(projectedFiles))
	for _, filePath := range projectedFiles {
		projected[filePath] = struct{}{}
	}
	accepted := make(map[string]struct{}, len(checked.Valid))
	for _, filePath := range checked.Valid {
		relativePath, err := filepath.Rel(coreDirectory, filePath)
		if err != nil {
			return fmt.Errorf("relativize module-zip path %s: %w", filePath, err)
		}
		normalized := filepath.ToSlash(relativePath)
		if !isSafeModulePath(normalized) {
			return fmt.Errorf("module-zip path escapes core: %s", filePath)
		}
		if err := validateTopLevelPath(normalized); err != nil {
			return err
		}
		accepted[normalized] = struct{}{}
	}
	if difference := setDifference(accepted, projected); len(difference) != 0 {
		return fmt.Errorf("core source has module-zip files outside the Git projection: %s", strings.Join(difference, ", "))
	}
	if difference := setDifference(projected, accepted); len(difference) != 0 {
		return fmt.Errorf("Git projection has files rejected by module-zip rules: %s", strings.Join(difference, ", "))
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

func stageFiles(repositoryRoot, stageDirectory string, files []string) error {
	for _, relativePath := range files {
		sourcePath := filepath.Join(repositoryRoot, "core", filepath.FromSlash(relativePath))
		destinationPath := filepath.Join(stageDirectory, filepath.FromSlash(relativePath))
		if err := os.MkdirAll(filepath.Dir(destinationPath), 0o755); err != nil {
			return fmt.Errorf("create staging parent for %s: %w", relativePath, err)
		}
		if err := copyRegularFile(sourcePath, destinationPath); err != nil {
			return err
		}
	}
	return nil
}

func copyRegularFile(sourcePath, destinationPath string) error {
	source, err := os.Open(sourcePath)
	if err != nil {
		return fmt.Errorf("open %s: %w", sourcePath, err)
	}
	defer source.Close()

	destination, err := os.OpenFile(destinationPath, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o644)
	if err != nil {
		return fmt.Errorf("create %s: %w", destinationPath, err)
	}
	_, copyErr := io.Copy(destination, source)
	closeErr := destination.Close()
	if copyErr != nil {
		return fmt.Errorf("copy %s: %w", sourcePath, copyErr)
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

	entries, err := os.ReadDir(vectorDirectory)
	if err != nil {
		return fmt.Errorf("read testvector directory: %w", err)
	}
	actual := make(map[string]struct{})
	for _, entry := range entries {
		if !entry.Type().IsRegular() || path.Ext(entry.Name()) != ".json" {
			continue
		}
		actual[entry.Name()] = struct{}{}
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

func validateModuleZipInput(stageDirectory string, projectedFiles []string) error {
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
	if len(checked.Valid) != len(projectedFiles) {
		return fmt.Errorf("module zip accepts %d files; projected source contains %d", len(checked.Valid), len(projectedFiles))
	}
	return nil
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
		return fmt.Errorf("construct module zip: %w", createErr)
	}
	if closeErr != nil {
		return fmt.Errorf("close module zip: %w", closeErr)
	}
	return nil
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
