package main

import (
	"archive/zip"
	"bytes"
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"strings"
	"testing"

	"golang.org/x/mod/module"
	modzip "golang.org/x/mod/zip"
)

const testModuleVersion = "v1.2.3"

func TestValidateConfiguration(t *testing.T) {
	t.Parallel()

	valid := configuration{
		repositoryRoot: "repo",
		commitSHA:      strings.Repeat("a", 40),
		stageDirectory: "stage",
		zipPath:        "module.zip",
		extractPath:    "extract",
		version:        testModuleVersion,
	}
	if err := validateConfiguration(valid); err != nil {
		t.Fatalf("valid configuration: %v", err)
	}

	cases := []struct {
		name   string
		mutate func(*configuration)
		want   string
	}{
		{name: "missing", mutate: func(c *configuration) { c.repositoryRoot = "" }, want: "-repo is required"},
		{name: "sha", mutate: func(c *configuration) { c.commitSHA = strings.Repeat("A", 40) }, want: "lowercase 40-character"},
		{name: "version", mutate: func(c *configuration) { c.version = "1.2.3" }, want: "invalid module version"},
	}
	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			config := valid
			tc.mutate(&config)
			if err := validateConfiguration(config); err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("validateConfiguration() error = %v, want containing %q", err, tc.want)
			}
		})
	}
}

func TestIsolatedGitEnvironment(t *testing.T) {
	t.Setenv("GIT_DIR", "redirected")
	t.Setenv("GIT_CONFIG_COUNT", "7")
	t.Setenv("WINDSHARE_TEST_SENTINEL", "present")

	environment := isolatedGitEnvironment()
	joined := strings.Join(environment, "\n")
	for _, forbidden := range []string{"GIT_DIR=redirected", "GIT_CONFIG_COUNT=7"} {
		if strings.Contains(joined, forbidden) {
			t.Fatalf("isolated environment retained %q", forbidden)
		}
	}
	for _, required := range []string{
		"WINDSHARE_TEST_SENTINEL=present",
		"GIT_CONFIG_NOSYSTEM=1",
		"GIT_CONFIG_GLOBAL=" + os.DevNull,
		"GIT_NO_REPLACE_OBJECTS=1",
		"GIT_TERMINAL_PROMPT=0",
	} {
		if !strings.Contains(joined, required) {
			t.Fatalf("isolated environment omitted %q", required)
		}
	}
}

func TestParseCommittedModuleFiles(t *testing.T) {
	t.Parallel()

	shaA := strings.Repeat("a", 40)
	shaB := strings.Repeat("b", 40)
	output := treeRecords(
		"100644 blob "+shaB+"\tcore/link/link.go",
		"100755 blob "+shaA+"\tscripts/ci/linux/release.sh",
	)
	files, err := parseCommittedModuleFiles(output)
	if err != nil {
		t.Fatalf("parseCommittedModuleFiles: %v", err)
	}
	want := []committedFile{
		{relativePath: "core/link/link.go", objectID: shaB},
		{relativePath: "scripts/ci/linux/release.sh", objectID: shaA},
	}
	if !reflect.DeepEqual(files, want) {
		t.Fatalf("files = %#v, want %#v", files, want)
	}

	cases := []struct {
		name   string
		record string
		want   string
	}{
		{name: "traversal", record: "100644 blob " + shaA + "\t../LICENSE", want: "invalid root module path"},
		{name: "backslash", record: "100644 blob " + shaA + "\tcore\\link.go", want: "non-canonical path"},
		{name: "symlink", record: "120000 blob " + shaA + "\tlink", want: "not a regular file"},
		{name: "tree", record: "040000 tree " + shaA + "\tcore", want: "not a regular file"},
		{name: "malformed", record: "core/link.go", want: "malformed tree record"},
	}
	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			_, err := parseCommittedModuleFiles(treeRecords(tc.record))
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("error = %v, want containing %q", err, tc.want)
			}
		})
	}
}

func TestCommittedModuleFilesIgnoreIndexAndWorktree(t *testing.T) {
	repositoryRoot, commitSHA := newModuleRepository(t)
	writeFile(t, filepath.Join(repositoryRoot, "README.md"), "mutated worktree\n")
	writeFile(t, filepath.Join(repositoryRoot, "untracked.txt"), "untracked\n")

	files, err := committedModuleFiles(repositoryRoot, commitSHA)
	if err != nil {
		t.Fatalf("committedModuleFiles: %v", err)
	}
	paths := committedFilePaths(files)
	if contains(paths, "untracked.txt") {
		t.Fatal("untracked file entered committed projection")
	}
	stage := filepath.Join(t.TempDir(), "stage")
	if err := os.Mkdir(stage, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := stageFiles(repositoryRoot, stage, files); err != nil {
		t.Fatalf("stageFiles: %v", err)
	}
	if got := readFile(t, filepath.Join(stage, "README.md")); got != "fixture readme\n" {
		t.Fatalf("staged README = %q", got)
	}
}

func TestAuditCommittedProjectionRequiresReleaseIdentity(t *testing.T) {
	t.Parallel()

	files := make([]committedFile, 0, len(requiredFiles))
	for _, filePath := range requiredFiles {
		files = append(files, committedFile{relativePath: filePath})
	}
	if err := auditCommittedProjection(files); err != nil {
		t.Fatalf("complete projection: %v", err)
	}
	files = files[1:]
	if err := auditCommittedProjection(files); err == nil || !strings.Contains(err.Error(), requiredFiles[0]) {
		t.Fatalf("incomplete projection error = %v", err)
	}
}

func TestValidateReleaseMetadataAndVectorInventory(t *testing.T) {
	t.Parallel()

	stage := t.TempDir()
	writeModuleFixture(t, stage)
	if err := validateReleaseMetadata(stage); err != nil {
		t.Fatalf("validateReleaseMetadata: %v", err)
	}

	writeFile(t, filepath.Join(stage, "core/testvectors/extra.json"), "{}\n")
	if err := validateReleaseMetadata(stage); err == nil || !strings.Contains(err.Error(), "missing inventory entries") {
		t.Fatalf("extra vector error = %v", err)
	}
}

func TestModuleZipInputUsesCanonicalNestedModuleOmissions(t *testing.T) {
	t.Parallel()

	stage := t.TempDir()
	writeModuleFixture(t, stage)
	accepted, err := validateModuleZipInput(stage)
	if err != nil {
		t.Fatalf("validateModuleZipInput: %v", err)
	}
	for _, omitted := range []string{
		"internal/perfevidence/go.mod",
		"internal/perfevidence/evidence.go",
		"spikes/webrtc/go.mod",
		"spikes/webrtc/spike.go",
	} {
		if contains(accepted, omitted) {
			t.Fatalf("nested module file entered root archive: %s", omitted)
		}
	}
	if !contains(accepted, "core/link/link.go") || !contains(accepted, "cmd/wind/main.go") {
		t.Fatalf("root production files missing from accepted projection: %v", accepted)
	}

	writeFile(t, filepath.Join(stage, "vendor/example.com/dep/dep.go"), "package dep\n")
	if _, err := validateModuleZipInput(stage); err == nil || !strings.Contains(err.Error(), "unexpectedly omits vendor/") {
		t.Fatalf("unreviewed omission error = %v", err)
	}
}

func TestRunBuildsDeterministicExactCommitArchive(t *testing.T) {
	repositoryRoot, commitSHA := newModuleRepository(t)
	first := newRunConfiguration(t, repositoryRoot, commitSHA, "first")
	second := newRunConfiguration(t, repositoryRoot, commitSHA, "second")

	if err := run(first); err != nil {
		t.Fatalf("first run: %v", err)
	}
	writeFile(t, filepath.Join(repositoryRoot, "README.md"), "uncommitted mutation\n")
	if err := run(second); err != nil {
		t.Fatalf("second run: %v", err)
	}
	firstDigest, err := fileDigest(first.zipPath)
	if err != nil {
		t.Fatal(err)
	}
	secondDigest, err := fileDigest(second.zipPath)
	if err != nil {
		t.Fatal(err)
	}
	if firstDigest != secondDigest {
		t.Fatalf("archive digest changed across worktree mutation: %x != %x", firstDigest, secondDigest)
	}
	if got := readFile(t, filepath.Join(second.extractPath, "README.md")); got != "fixture readme\n" {
		t.Fatalf("archive used worktree bytes: %q", got)
	}
	for _, omitted := range []string{"internal/perfevidence/evidence.go", "spikes/webrtc/spike.go"} {
		if _, err := os.Stat(filepath.Join(second.extractPath, filepath.FromSlash(omitted))); !os.IsNotExist(err) {
			t.Fatalf("nested module path %s was extracted: %v", omitted, err)
		}
	}

	version := module.Version{Path: modulePath, Version: testModuleVersion}
	checked, err := modzip.CheckZip(version, second.zipPath)
	if err != nil {
		t.Fatalf("CheckZip: %v", err)
	}
	if checked.Err() != nil {
		t.Fatalf("archive contract: %v", checked.Err())
	}
	assertCanonicalZipPrefix(t, second.zipPath, modulePath+"@"+testModuleVersion+"/")
}

func TestRunRemovesFailedArchiveWithoutDeletingUnownedFile(t *testing.T) {
	repositoryRoot, commitSHA := newModuleRepository(t)
	config := newRunConfiguration(t, repositoryRoot, commitSHA, "failure")
	writeFile(t, config.zipPath, "owned by caller")
	if err := run(config); err == nil || !strings.Contains(err.Error(), "create module zip") {
		t.Fatalf("run error = %v", err)
	}
	if got := readFile(t, config.zipPath); got != "owned by caller" {
		t.Fatalf("run changed unowned zip: %q", got)
	}
}

func newRunConfiguration(t *testing.T, repositoryRoot, commitSHA, name string) configuration {
	t.Helper()
	root := filepath.Join(t.TempDir(), name)
	return configuration{
		repositoryRoot: repositoryRoot,
		commitSHA:      commitSHA,
		stageDirectory: filepath.Join(root, "stage"),
		zipPath:        filepath.Join(root, "module.zip"),
		extractPath:    filepath.Join(root, "extract"),
		version:        testModuleVersion,
	}
}

func newModuleRepository(t *testing.T) (string, string) {
	t.Helper()
	repositoryRoot := t.TempDir()
	runGit(t, repositoryRoot, "init", "--quiet")
	runGit(t, repositoryRoot, "config", "user.email", "release-tests@example.invalid")
	runGit(t, repositoryRoot, "config", "user.name", "Release Tests")
	writeModuleFixture(t, repositoryRoot)
	runGit(t, repositoryRoot, "add", ".")
	runGit(t, repositoryRoot, "commit", "--quiet", "-m", "fixture")
	commitSHA := strings.TrimSpace(gitOutputForTest(t, repositoryRoot, "rev-parse", "HEAD"))
	return repositoryRoot, commitSHA
}

func writeModuleFixture(t *testing.T, root string) {
	t.Helper()
	files := map[string]string{
		".testcoverage.yml":                 "threshold: 1\n",
		"LICENSE":                           "Apache License\nVersion 2.0\n",
		"NOTICE":                            "WindShare\n",
		"README.md":                         "fixture readme\n",
		"go.mod":                            "module " + modulePath + "\n\ngo 1.25.0\n",
		"go.sum":                            "example.com/dependency v1.0.0 h1:AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA=\n",
		"cmd/wind/main.go":                  "package main\nfunc main() {}\n",
		"core/link/link.go":                 "package link\n",
		"core/testvectors/README.md":        "vectors\n",
		"core/testvectors/inventory.txt":    "canonical.json\n",
		"core/testvectors/canonical.json":   "{}\n",
		"internal/perfevidence/go.mod":      "module example.com/perfevidence\n\ngo 1.25.0\n",
		"internal/perfevidence/evidence.go": "package perfevidence\n",
		"spikes/webrtc/go.mod":              "module example.com/webrtc\n\ngo 1.25.0\n",
		"spikes/webrtc/spike.go":            "package webrtc\n",
	}
	for filePath, content := range files {
		writeFile(t, filepath.Join(root, filepath.FromSlash(filePath)), content)
	}
}

func assertCanonicalZipPrefix(t *testing.T, zipPath, prefix string) {
	t.Helper()
	reader, err := zip.OpenReader(zipPath)
	if err != nil {
		t.Fatal(err)
	}
	defer reader.Close()
	if len(reader.File) == 0 {
		t.Fatal("module archive is empty")
	}
	for _, file := range reader.File {
		if !strings.HasPrefix(file.Name, prefix) {
			t.Fatalf("zip entry %q lacks prefix %q", file.Name, prefix)
		}
	}
}

func treeRecords(records ...string) []byte {
	return append([]byte(strings.Join(records, "\x00")), 0)
}

func contains(values []string, target string) bool {
	index := sort.SearchStrings(values, target)
	return index < len(values) && values[index] == target
}

func writeFile(t *testing.T, filePath, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(filePath), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filePath, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

func readFile(t *testing.T, filePath string) string {
	t.Helper()
	content, err := os.ReadFile(filePath)
	if err != nil {
		t.Fatal(err)
	}
	return string(content)
}

func runGit(t *testing.T, repositoryRoot string, arguments ...string) {
	t.Helper()
	if _, err := gitOutputForTestWithError(repositoryRoot, arguments...); err != nil {
		t.Fatalf("git %s: %v", strings.Join(arguments, " "), err)
	}
}

func gitOutputForTest(t *testing.T, repositoryRoot string, arguments ...string) string {
	t.Helper()
	output, err := gitOutputForTestWithError(repositoryRoot, arguments...)
	if err != nil {
		t.Fatalf("git %s: %v", strings.Join(arguments, " "), err)
	}
	return string(output)
}

func gitOutputForTestWithError(repositoryRoot string, arguments ...string) ([]byte, error) {
	return gitOutput(repositoryRoot, arguments...)
}

func TestValidateExactProjection(t *testing.T) {
	t.Parallel()
	if err := validateExactProjection([]string{"a", "b"}, []string{"b", "a"}, "archive"); err != nil {
		t.Fatalf("same set: %v", err)
	}
	if err := validateExactProjection([]string{"a"}, []string{"a", "b"}, "archive"); err == nil {
		t.Fatal("missing committed file was accepted")
	}
	if err := validateExactProjection([]string{"a", "a"}, []string{"a"}, "archive"); err == nil {
		t.Fatal("duplicate archive path was accepted")
	}
}

func TestCreateModuleZipRejectsExistingOutput(t *testing.T) {
	t.Parallel()
	stage := t.TempDir()
	writeFile(t, filepath.Join(stage, "go.mod"), "module "+modulePath+"\n")
	zipPath := filepath.Join(t.TempDir(), "module.zip")
	writeFile(t, zipPath, "sentinel")
	err := createModuleZip(zipPath, module.Version{Path: modulePath, Version: testModuleVersion}, stage)
	if err == nil || !strings.Contains(err.Error(), "create module zip") {
		t.Fatalf("createModuleZip error = %v", err)
	}
	if got := readFile(t, zipPath); got != "sentinel" {
		t.Fatalf("existing output changed: %q", got)
	}
}

func TestFileDigest(t *testing.T) {
	t.Parallel()
	filePath := filepath.Join(t.TempDir(), "value")
	writeFile(t, filePath, "windshare")
	first, err := fileDigest(filePath)
	if err != nil {
		t.Fatal(err)
	}
	second, err := fileDigest(filePath)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(first[:], second[:]) {
		t.Fatal("stable file produced unstable digest")
	}
}
