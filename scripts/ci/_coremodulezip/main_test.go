package main

import (
	"bytes"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"sort"
	"strings"
	"testing"

	"golang.org/x/mod/module"
	modzip "golang.org/x/mod/zip"
)

const testModuleVersion = "v0.3.0"

func TestValidateConfiguration(t *testing.T) {
	valid := configuration{
		repositoryRoot: "repo",
		commitSHA:      strings.Repeat("a", 40),
		stageDirectory: "stage",
		zipPath:        "core.zip",
		extractPath:    "extract",
		version:        testModuleVersion,
	}
	if err := validateConfiguration(valid); err != nil {
		t.Fatalf("validateConfiguration(valid): %v", err)
	}

	tests := []struct {
		name   string
		mutate func(*configuration)
		want   string
	}{
		{name: "repo", mutate: func(config *configuration) { config.repositoryRoot = " " }, want: "-repo is required"},
		{name: "commit", mutate: func(config *configuration) { config.commitSHA = "" }, want: "-commit is required"},
		{name: "abbreviated commit", mutate: func(config *configuration) { config.commitSHA = "abc123" }, want: "exact lowercase"},
		{name: "uppercase commit", mutate: func(config *configuration) { config.commitSHA = strings.Repeat("A", 40) }, want: "exact lowercase"},
		{name: "stage", mutate: func(config *configuration) { config.stageDirectory = "" }, want: "-stage is required"},
		{name: "zip", mutate: func(config *configuration) { config.zipPath = "" }, want: "-zip is required"},
		{name: "extract", mutate: func(config *configuration) { config.extractPath = "" }, want: "-extract is required"},
		{name: "version", mutate: func(config *configuration) { config.version = "" }, want: "-version is required"},
		{name: "invalid version", mutate: func(config *configuration) { config.version = "release-3" }, want: "invalid module version"},
		{name: "wrong major", mutate: func(config *configuration) { config.version = "v2.0.0" }, want: "invalid module version"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			config := valid
			test.mutate(&config)
			requireErrorContains(t, validateConfiguration(config), test.want)
		})
	}
}

func TestIsolatedGitEnvironment(t *testing.T) {
	t.Setenv("GIT_DIR", "redirected")
	t.Setenv("GIT_WORK_TREE", "redirected")
	t.Setenv("GIT_CONFIG_GLOBAL", "caller-config")

	allowed := map[string]string{
		"GIT_CONFIG_NOSYSTEM":    "1",
		"GIT_CONFIG_GLOBAL":      os.DevNull,
		"GIT_NO_REPLACE_OBJECTS": "1",
		"GIT_TERMINAL_PROMPT":    "0",
	}
	for _, variable := range isolatedGitEnvironment() {
		key, value, _ := strings.Cut(variable, "=")
		normalized := strings.ToUpper(key)
		if !strings.HasPrefix(normalized, "GIT_") {
			continue
		}
		want, ok := allowed[normalized]
		if !ok {
			t.Fatalf("isolatedGitEnvironment retained %s", key)
		}
		if value != want {
			t.Fatalf("%s = %q, want %q", key, value, want)
		}
		delete(allowed, normalized)
	}
	if len(allowed) != 0 {
		t.Fatalf("isolatedGitEnvironment omitted controls: %v", allowed)
	}
}

func TestSafeModulePathAndTopLevelPolicy(t *testing.T) {
	for _, filePath := range []string{
		".testcoverage.yml",
		"catalog/file.go",
		"testvectors/vector.json",
		"internal/protocol/file.go",
	} {
		if !isSafeModulePath(filePath) {
			t.Errorf("isSafeModulePath(%q) = false", filePath)
		}
	}

	for _, filePath := range []string{
		"",
		".",
		"..",
		"../LICENSE",
		"/LICENSE",
		"catalog/../LICENSE",
		"catalog//file.go",
		"catalog/./file.go",
		"catalog\\file.go",
		"catalog/file:stream",
		"catalog/CON",
	} {
		if isSafeModulePath(filePath) {
			t.Errorf("isSafeModulePath(%q) = true", filePath)
		}
	}

	tests := []struct {
		path string
		want string
	}{
		{path: "README.md"},
		{path: "catalog/file.go"},
		{path: "rogue.txt", want: "unexpected top-level core file"},
		{path: "build/file.go", want: "unexpected top-level core directory"},
		{path: "catalog/", want: "invalid empty path"},
	}
	for _, test := range tests {
		err := validateTopLevelPath(test.path)
		if test.want == "" {
			if err != nil {
				t.Errorf("validateTopLevelPath(%q): %v", test.path, err)
			}
			continue
		}
		requireErrorContains(t, err, test.want)
	}
}

func TestCommittedCoreFilesIgnoreIndexAndWorktree(t *testing.T) {
	repositoryRoot := newValidRepository(t)
	commitSHA := repositoryHead(t, repositoryRoot)
	writeFile(t, filepath.Join(repositoryRoot, ".gitignore"), "core/transfer/ignored.go\n")
	writeFile(t, filepath.Join(repositoryRoot, "core", "README.md"), "mutated after clean\n")
	writeFile(t, filepath.Join(repositoryRoot, "core", "transfer", "untracked.go"), "package transfer\n")
	writeFile(t, filepath.Join(repositoryRoot, "core", "transfer", "ignored.go"), "package transfer\n")
	runGit(t, repositoryRoot, "add", "core/README.md", "core/transfer/untracked.go")
	if err := os.Remove(filepath.Join(repositoryRoot, "core", "link", "fixture.txt")); err != nil {
		t.Fatal(err)
	}
	t.Setenv("GIT_DIR", filepath.Join(t.TempDir(), "redirected-git-dir"))
	t.Setenv("GIT_INDEX_FILE", filepath.Join(t.TempDir(), "redirected-index"))

	files, err := committedCoreFiles(repositoryRoot, commitSHA)
	if err != nil {
		t.Fatalf("committedCoreFiles: %v", err)
	}
	paths := committedFilePaths(files)
	if !sort.StringsAreSorted(paths) {
		t.Fatalf("committed projection is not sorted: %v", paths)
	}
	requireContainsPath(t, paths, "transfer/untracked.go", false)
	requireContainsPath(t, paths, "transfer/ignored.go", false)
	requireContainsPath(t, paths, "link/fixture.txt", true)
	requireContainsPath(t, paths, "catalog/fixture.txt", true)
}

func TestCommittedCoreFilesFailClosed(t *testing.T) {
	_, err := committedCoreFiles(t.TempDir(), strings.Repeat("a", 40))
	requireErrorContains(t, err, "inspect release commit")

	repositoryRoot := newValidRepository(t)
	blobSHA := strings.TrimSpace(gitOutputForTest(t, repositoryRoot, "hash-object", "core/README.md"))
	_, err = committedCoreFiles(repositoryRoot, blobSHA)
	requireErrorContains(t, err, "directly identify a commit")

	tests := []struct {
		name   string
		output []byte
		want   string
	}{
		{name: "outside core", output: treeRecords("100644 blob " + strings.Repeat("a", 40) + "\tREADME.md"), want: "outside core"},
		{name: "traversal", output: treeRecords("100644 blob " + strings.Repeat("a", 40) + "\tcore/../LICENSE"), want: "invalid core module path"},
		{name: "backslash", output: treeRecords("100644 blob " + strings.Repeat("a", 40) + "\tcore/catalog\\file.go"), want: "non-canonical path"},
		{name: "nonportable", output: treeRecords("100644 blob " + strings.Repeat("a", 40) + "\tcore/catalog/file:stream"), want: "invalid core module path"},
		{name: "symlink", output: treeRecords("120000 blob " + strings.Repeat("a", 40) + "\tcore/catalog/file.go"), want: "not a regular file"},
		{name: "tree", output: treeRecords("040000 tree " + strings.Repeat("a", 40) + "\tcore/catalog"), want: "not a regular file"},
		{name: "malformed", output: treeRecords("core/catalog/file.go"), want: "malformed tree record"},
		{name: "unexpected", output: treeRecords("100644 blob " + strings.Repeat("a", 40) + "\tcore/rogue.txt"), want: "unexpected top-level core file"},
		{name: "duplicate", output: treeRecords(
			"100644 blob "+strings.Repeat("a", 40)+"\tcore/LICENSE",
			"100644 blob "+strings.Repeat("b", 40)+"\tcore/LICENSE",
		), want: "duplicate core module path"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			_, err := parseCommittedCoreFiles(test.output)
			requireErrorContains(t, err, test.want)
		})
	}
}

func TestAuditCommittedProjection(t *testing.T) {
	repositoryRoot := newValidRepository(t)
	files := committedOrFatal(t, repositoryRoot)
	if err := auditCommittedProjection(files); err != nil {
		t.Fatalf("auditCommittedProjection: %v", err)
	}

	withoutNotice := removeCommittedPath(files, "NOTICE")
	requireErrorContains(t, auditCommittedProjection(withoutNotice), "required top-level core files are missing")
	withoutLiveshare := removeCommittedPrefix(files, "liveshare/")
	requireErrorContains(t, auditCommittedProjection(withoutLiveshare), "required top-level core directories are missing")
}

func TestStageFilesReadsCommittedBlobs(t *testing.T) {
	repositoryRoot := newValidRepository(t)
	files := committedOrFatal(t, repositoryRoot)
	selected := selectCommittedFiles(t, files, "catalog/fixture.txt", "testvectors/vector.json")
	writeFile(t, filepath.Join(repositoryRoot, "core", "catalog", "fixture.txt"), "worktree mutation\n")
	stageDirectory := t.TempDir()
	if err := stageFiles(repositoryRoot, stageDirectory, selected); err != nil {
		t.Fatalf("stageFiles: %v", err)
	}
	if got := readFile(t, filepath.Join(stageDirectory, "catalog", "fixture.txt")); got != "fixture for catalog\n" {
		t.Fatalf("staged committed bytes = %q", got)
	}

	escapePath := filepath.Join(filepath.Dir(stageDirectory), "escape.txt")
	requireErrorContains(t, stageFiles(repositoryRoot, stageDirectory, []committedFile{{relativePath: "../escape.txt"}}), "invalid staged module path")
	if _, err := os.Stat(escapePath); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("stage path escaped: %v", err)
	}
	requireErrorContains(t, stageFiles(repositoryRoot, stageDirectory, []committedFile{{relativePath: "rogue/file.go"}}), "unexpected top-level core directory")
	requireErrorContains(t, stageFiles(repositoryRoot, stageDirectory, []committedFile{{relativePath: "catalog/missing.go", objectID: strings.Repeat("a", 40)}}), "read committed blob")

	existingStage := t.TempDir()
	existing := filepath.Join(existingStage, "catalog", "fixture.txt")
	writeFile(t, existing, "do not replace")
	requireErrorContains(t, stageFiles(repositoryRoot, existingStage, selected[:1]), "create")
	if got := readFile(t, existing); got != "do not replace" {
		t.Fatalf("existing stage file changed: %q", got)
	}
}

func TestReleaseMetadata(t *testing.T) {
	t.Run("valid", func(t *testing.T) {
		stageDirectory := newValidStage(t)
		if err := validateReleaseMetadata(stageDirectory); err != nil {
			t.Fatalf("validateReleaseMetadata: %v", err)
		}
	})

	tests := []struct {
		name   string
		mutate func(*testing.T, string)
		want   string
	}{
		{
			name: "missing required file",
			mutate: func(t *testing.T, stage string) {
				if err := os.Remove(filepath.Join(stage, "README.md")); err != nil {
					t.Fatal(err)
				}
			},
			want: "required release file README.md",
		},
		{
			name: "empty required file",
			mutate: func(t *testing.T, stage string) {
				if err := os.WriteFile(filepath.Join(stage, ".testcoverage.yml"), nil, 0o644); err != nil {
					t.Fatal(err)
				}
			},
			want: "empty or irregular",
		},
		{
			name: "irregular required file",
			mutate: func(t *testing.T, stage string) {
				target := filepath.Join(stage, "NOTICE")
				if err := os.Remove(target); err != nil {
					t.Fatal(err)
				}
				if err := os.Mkdir(target, 0o755); err != nil {
					t.Fatal(err)
				}
			},
			want: "empty or irregular",
		},
		{
			name: "wrong module",
			mutate: func(t *testing.T, stage string) {
				writeFile(t, filepath.Join(stage, "go.mod"), "module example.com/wrong\n")
			},
			want: "go.mod module path",
		},
		{
			name: "wrong license",
			mutate: func(t *testing.T, stage string) {
				writeFile(t, filepath.Join(stage, "LICENSE"), "Apache License")
			},
			want: "Version 2.0",
		},
		{
			name: "wrong notice",
			mutate: func(t *testing.T, stage string) {
				writeFile(t, filepath.Join(stage, "NOTICE"), "Another project")
			},
			want: "WindShare",
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			stageDirectory := newValidStage(t)
			test.mutate(t, stageDirectory)
			requireErrorContains(t, validateReleaseMetadata(stageDirectory), test.want)
		})
	}
}

func TestVectorInventoryIsExactAndFlat(t *testing.T) {
	tests := []struct {
		name   string
		mutate func(*testing.T, string)
		want   string
	}{
		{
			name: "comments and whitespace",
			mutate: func(t *testing.T, stage string) {
				writeFile(t, filepath.Join(stage, "testvectors", "inventory.txt"), "# vectors\n\n vector.json \n")
			},
		},
		{
			name: "missing inventory",
			mutate: func(t *testing.T, stage string) {
				if err := os.Remove(filepath.Join(stage, "testvectors", "inventory.txt")); err != nil {
					t.Fatal(err)
				}
			},
			want: "open testvector inventory",
		},
		{
			name: "empty inventory",
			mutate: func(t *testing.T, stage string) {
				writeFile(t, filepath.Join(stage, "testvectors", "inventory.txt"), "# none\n")
			},
			want: "inventory is empty",
		},
		{
			name: "nested inventory entry",
			mutate: func(t *testing.T, stage string) {
				writeFile(t, filepath.Join(stage, "testvectors", "inventory.txt"), "nested/vector.json\n")
			},
			want: "invalid testvector inventory entry",
		},
		{
			name: "non-json inventory entry",
			mutate: func(t *testing.T, stage string) {
				writeFile(t, filepath.Join(stage, "testvectors", "inventory.txt"), "vector.txt\n")
			},
			want: "invalid testvector inventory entry",
		},
		{
			name: "duplicate inventory entry",
			mutate: func(t *testing.T, stage string) {
				writeFile(t, filepath.Join(stage, "testvectors", "inventory.txt"), "vector.json\nvector.json\n")
			},
			want: "duplicate testvector inventory entry",
		},
		{
			name: "listed file missing",
			mutate: func(t *testing.T, stage string) {
				if err := os.Remove(filepath.Join(stage, "testvectors", "vector.json")); err != nil {
					t.Fatal(err)
				}
			},
			want: "inventory names missing files",
		},
		{
			name: "unlisted file",
			mutate: func(t *testing.T, stage string) {
				writeFile(t, filepath.Join(stage, "testvectors", "extra.json"), "{}")
			},
			want: "JSON files missing inventory entries",
		},
		{
			name: "nested json",
			mutate: func(t *testing.T, stage string) {
				writeFile(t, filepath.Join(stage, "testvectors", "nested", "extra.json"), "{}")
			},
			want: "JSON files missing inventory entries",
		},
		{
			name: "irregular json",
			mutate: func(t *testing.T, stage string) {
				if err := os.Mkdir(filepath.Join(stage, "testvectors", "extra.json"), 0o755); err != nil {
					t.Fatal(err)
				}
			},
			want: "testvector JSON path is irregular",
		},
		{
			name: "scanner limit",
			mutate: func(t *testing.T, stage string) {
				writeFile(t, filepath.Join(stage, "testvectors", "inventory.txt"), strings.Repeat("a", 70*1024)+".json\n")
			},
			want: "read testvector inventory",
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			stageDirectory := newValidStage(t)
			test.mutate(t, stageDirectory)
			err := validateVectorInventory(stageDirectory)
			if test.want == "" {
				if err != nil {
					t.Fatalf("validateVectorInventory: %v", err)
				}
				return
			}
			requireErrorContains(t, err, test.want)
		})
	}
}

func TestModuleZipInputAndArchiveHelpers(t *testing.T) {
	stageDirectory := newValidStage(t)
	files := collectRegularFiles(t, stageDirectory)
	if err := validateModuleZipInput(stageDirectory, files); err != nil {
		t.Fatalf("validateModuleZipInput: %v", err)
	}

	requireErrorContains(
		t,
		validateModuleZipInput(stageDirectory, append(files, "transfer/missing.go")),
		"absent from the staged module",
	)
	equalCountMismatch := append([]string(nil), files...)
	equalCountMismatch[0] = "transfer/missing.go"
	requireErrorContains(t, validateModuleZipInput(stageDirectory, equalCountMismatch), "outside the committed projection")
	requireErrorContains(t, validateModuleZipInput(stageDirectory, append(files, files[0])), "committed projection contains duplicate module path")
	invalidProjection := append([]string(nil), files...)
	invalidProjection[0] = "../escape.go"
	requireErrorContains(t, validateModuleZipInput(stageDirectory, invalidProjection), "committed projection contains invalid module path")

	omittedStage := newValidStage(t)
	writeFile(t, filepath.Join(omittedStage, ".git", "config"), "ignored")
	requireErrorContains(t, validateModuleZipInput(omittedStage, collectRegularFiles(t, omittedStage)), "would omit projected files")

	version := module.Version{Path: modulePath, Version: testModuleVersion}
	zipPath := filepath.Join(t.TempDir(), "core.zip")
	if err := createModuleZip(zipPath, version, stageDirectory); err != nil {
		t.Fatalf("createModuleZip: %v", err)
	}
	checked, err := modzip.CheckZip(version, zipPath)
	if err != nil {
		t.Fatalf("CheckZip: %v", err)
	}
	if err := validateModuleZipProjection(version, checked.Valid, files); err != nil {
		t.Fatalf("validateModuleZipProjection: %v", err)
	}
	substituted := append([]string(nil), checked.Valid...)
	substituted[0] = modulePath + "@" + testModuleVersion + "/transfer/substituted.go"
	requireErrorContains(t, validateModuleZipProjection(version, substituted, files), "module zip has files outside the committed projection")
	requireErrorContains(t, validateModuleZipProjection(version, []string{"wrong-prefix/file.go"}, files), "lacks canonical prefix")
	for _, wrong := range []module.Version{
		{Path: "example.com/wrong/core", Version: testModuleVersion},
		{Path: modulePath, Version: "v0.3.1"},
	} {
		if _, err := modzip.CheckZip(wrong, zipPath); err == nil {
			t.Errorf("CheckZip accepted archive as %s@%s", wrong.Path, wrong.Version)
		}
	}
	digest, err := fileDigest(zipPath)
	if err != nil {
		t.Fatalf("fileDigest: %v", err)
	}
	if digest == ([32]byte{}) {
		t.Fatal("fileDigest returned zero digest")
	}

	preexisting := filepath.Join(t.TempDir(), "existing.zip")
	writeFile(t, preexisting, "owned elsewhere")
	requireErrorContains(t, createModuleZip(preexisting, version, stageDirectory), "create module zip")
	if got := readFile(t, preexisting); got != "owned elsewhere" {
		t.Fatalf("preexisting archive changed: %q", got)
	}

	failedPath := filepath.Join(t.TempDir(), "failed.zip")
	requireErrorContains(t, createModuleZip(failedPath, version, filepath.Join(t.TempDir(), "missing")), "construct module zip")
	if _, err := os.Stat(failedPath); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("failed archive was not removed: %v", err)
	}
	_, err = fileDigest(filepath.Join(t.TempDir(), "missing.zip"))
	requireErrorContains(t, err, "open archive for hashing")
}

func TestDirectoryAndCleanupHelpers(t *testing.T) {
	empty := t.TempDir()
	if err := requireEmptyDirectory(empty); err != nil {
		t.Fatalf("requireEmptyDirectory(empty): %v", err)
	}
	writeFile(t, filepath.Join(empty, "file"), "content")
	requireErrorContains(t, requireEmptyDirectory(empty), "not empty")
	requireErrorContains(t, requireEmptyDirectory(filepath.Join(t.TempDir(), "missing")), "read staging directory")

	sentinel := errors.New("original failure")
	owned := filepath.Join(t.TempDir(), "owned.zip")
	writeFile(t, owned, "partial")
	if err := removeFailedArchive(owned, sentinel); !errors.Is(err, sentinel) {
		t.Fatalf("removeFailedArchive lost original error: %v", err)
	}
	if _, err := os.Stat(owned); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("owned archive still exists: %v", err)
	}

	nonexistent := filepath.Join(t.TempDir(), "absent.zip")
	if err := removeFailedArchive(nonexistent, sentinel); !errors.Is(err, sentinel) {
		t.Fatalf("nonexistent cleanup lost original error: %v", err)
	}

	nonemptyDirectory := filepath.Join(t.TempDir(), "archive")
	writeFile(t, filepath.Join(nonemptyDirectory, "child"), "content")
	err := removeFailedArchive(nonemptyDirectory, sentinel)
	if !errors.Is(err, sentinel) || !strings.Contains(err.Error(), "remove failed module zip") {
		t.Fatalf("removeFailedArchive cleanup error = %v", err)
	}
}

func TestRunRejectsInvalidProjectionBeforePublishing(t *testing.T) {
	t.Run("empty committed projection", func(t *testing.T) {
		repositoryRoot := t.TempDir()
		runGit(t, repositoryRoot, "init", "--quiet")
		runGit(t, repositoryRoot, "-c", "user.name=WindShare release test", "-c", "user.email=release-test@invalid.example", "commit", "--quiet", "--allow-empty", "-m", "empty")
		config := artifactConfiguration(t, repositoryRoot, t.TempDir())
		requireErrorContains(t, run(config), "release commit contains no core files")
		requireNotExist(t, config.stageDirectory)
		requireNotExist(t, config.zipPath)
		requireNotExist(t, config.extractPath)
	})

	t.Run("missing required committed file", func(t *testing.T) {
		repositoryRoot := newValidRepository(t)
		if err := os.Remove(filepath.Join(repositoryRoot, "core", "NOTICE")); err != nil {
			t.Fatal(err)
		}
		commitWorktree(t, repositoryRoot, "remove notice")
		config := artifactConfiguration(t, repositoryRoot, t.TempDir())
		requireErrorContains(t, run(config), "required top-level core files are missing")
		requireNotExist(t, config.stageDirectory)
		requireNotExist(t, config.zipPath)
		requireNotExist(t, config.extractPath)
	})

	t.Run("unexpected top-level file", func(t *testing.T) {
		repositoryRoot := newValidRepository(t)
		writeFile(t, filepath.Join(repositoryRoot, "core", "debug.log"), "debug")
		commitWorktree(t, repositoryRoot, "add unexpected file")
		config := artifactConfiguration(t, repositoryRoot, t.TempDir())
		requireErrorContains(t, run(config), "unexpected top-level core file")
		requireNotExist(t, config.stageDirectory)
		requireNotExist(t, config.zipPath)
	})

	t.Run("wrong staged module", func(t *testing.T) {
		repositoryRoot := newValidRepository(t)
		writeFile(t, filepath.Join(repositoryRoot, "core", "go.mod"), "module example.com/wrong\n")
		commitWorktree(t, repositoryRoot, "change module")
		config := artifactConfiguration(t, repositoryRoot, t.TempDir())
		requireErrorContains(t, run(config), "go.mod module path")
		requireNotExist(t, config.zipPath)
		requireNotExist(t, config.extractPath)
	})

	t.Run("nonempty staging directory", func(t *testing.T) {
		repositoryRoot := newValidRepository(t)
		config := artifactConfiguration(t, repositoryRoot, t.TempDir())
		sentinel := filepath.Join(config.stageDirectory, "sentinel")
		writeFile(t, sentinel, "owned elsewhere")
		requireErrorContains(t, run(config), "staging directory is not empty")
		if got := readFile(t, sentinel); got != "owned elsewhere" {
			t.Fatalf("staging sentinel changed: %q", got)
		}
		requireNotExist(t, config.zipPath)
		requireNotExist(t, config.extractPath)
	})
}

func TestRunBuildsDeterministicExtractedArtifact(t *testing.T) {
	repositoryRoot := newValidRepository(t)
	first := runArtifact(t, repositoryRoot, t.TempDir())
	second := runArtifact(t, repositoryRoot, t.TempDir())

	firstZip := mustReadBytes(t, first.zipPath)
	secondZip := mustReadBytes(t, second.zipPath)
	if !bytes.Equal(firstZip, secondZip) {
		t.Fatal("equivalent projections produced different module zips")
	}
	if !reflect.DeepEqual(treeSnapshot(t, first.extractPath), treeSnapshot(t, second.extractPath)) {
		t.Fatal("equivalent module zips extracted differently")
	}
	if _, err := os.Stat(first.zipPath + ".determinism-check"); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("determinism-check archive leaked: %v", err)
	}

	version := module.Version{Path: modulePath, Version: testModuleVersion}
	checked, err := modzip.CheckZip(version, first.zipPath)
	if err != nil {
		t.Fatalf("CheckZip: %v", err)
	}
	if err := validateModuleZipProjection(version, checked.Valid, committedFilePaths(committedOrFatal(t, repositoryRoot))); err != nil {
		t.Fatalf("final archive projection: %v", err)
	}
	if got := readFile(t, filepath.Join(first.extractPath, "go.mod")); !strings.Contains(got, "module "+modulePath) {
		t.Fatalf("extracted go.mod = %q", got)
	}
}

func TestRunBindsCommittedBytesAfterCleanWorktreeMutation(t *testing.T) {
	repositoryRoot := newValidRepository(t)
	commitSHA := repositoryHead(t, repositoryRoot)
	if status := gitOutputForTest(t, repositoryRoot, "status", "--porcelain=v1", "--untracked-files=all"); status != "" {
		t.Fatalf("fixture was not clean before mutation: %q", status)
	}
	expectedREADME := readFile(t, filepath.Join(repositoryRoot, "core", "README.md"))
	writeFile(t, filepath.Join(repositoryRoot, "core", "README.md"), "tracked mutation after clean proof\n")
	writeFile(t, filepath.Join(repositoryRoot, "core", "transfer", "untracked-after-clean.go"), "package transfer\n")
	runGit(t, repositoryRoot, "add", "core/README.md")

	artifact := runArtifact(t, repositoryRoot, t.TempDir())
	if got := readFile(t, filepath.Join(artifact.extractPath, "README.md")); got != expectedREADME {
		t.Fatalf("archive used tracked worktree/index bytes: %q", got)
	}
	if _, err := os.Stat(filepath.Join(artifact.extractPath, "transfer", "untracked-after-clean.go")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("archive included an untracked worktree file: %v", err)
	}
	if config := artifactConfiguration(t, repositoryRoot, t.TempDir()); config.commitSHA != commitSHA {
		t.Fatalf("artifact configuration commit = %s, want %s", config.commitSHA, commitSHA)
	}
}

func TestRunDoesNotPublishOrDeleteUnownedArtifactsOnFailure(t *testing.T) {
	t.Run("invalid version mutates nothing", func(t *testing.T) {
		repositoryRoot := newValidRepository(t)
		root := t.TempDir()
		config := artifactConfiguration(t, repositoryRoot, root)
		config.version = "not-semver"
		requireErrorContains(t, run(config), "invalid module version")
		for _, target := range []string{config.stageDirectory, config.zipPath, config.extractPath} {
			if _, err := os.Stat(target); !errors.Is(err, os.ErrNotExist) {
				t.Fatalf("%s unexpectedly exists: %v", target, err)
			}
		}
	})

	t.Run("preexisting primary archive", func(t *testing.T) {
		repositoryRoot := newValidRepository(t)
		config := artifactConfiguration(t, repositoryRoot, t.TempDir())
		writeFile(t, config.zipPath, "owned elsewhere")
		requireErrorContains(t, run(config), "create module zip")
		if got := readFile(t, config.zipPath); got != "owned elsewhere" {
			t.Fatalf("preexisting primary archive changed: %q", got)
		}
	})

	t.Run("preexisting determinism archive", func(t *testing.T) {
		repositoryRoot := newValidRepository(t)
		config := artifactConfiguration(t, repositoryRoot, t.TempDir())
		determinismPath := config.zipPath + ".determinism-check"
		writeFile(t, determinismPath, "owned elsewhere")
		requireErrorContains(t, run(config), "create module zip")
		if got := readFile(t, determinismPath); got != "owned elsewhere" {
			t.Fatalf("preexisting determinism archive changed: %q", got)
		}
		if _, err := os.Stat(config.zipPath); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("failed primary archive was published: %v", err)
		}
	})

	t.Run("failed extraction", func(t *testing.T) {
		repositoryRoot := newValidRepository(t)
		config := artifactConfiguration(t, repositoryRoot, t.TempDir())
		writeFile(t, filepath.Join(config.extractPath, "sentinel"), "owned elsewhere")
		requireErrorContains(t, run(config), "extract module zip")
		if got := readFile(t, filepath.Join(config.extractPath, "sentinel")); got != "owned elsewhere" {
			t.Fatalf("preexisting extraction changed: %q", got)
		}
		if _, err := os.Stat(config.zipPath); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("archive survived failed extraction: %v", err)
		}
		if _, err := os.Stat(config.zipPath + ".determinism-check"); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("determinism archive survived failed extraction: %v", err)
		}
	})
}

func TestSetDifferenceIsSorted(t *testing.T) {
	left := map[string]struct{}{"z": {}, "a": {}, "m": {}}
	right := map[string]struct{}{"m": {}}
	if got, want := setDifference(left, right), []string{"a", "z"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("setDifference = %v, want %v", got, want)
	}
}

type artifactResult struct {
	zipPath     string
	extractPath string
}

func runArtifact(t *testing.T, repositoryRoot, artifactRoot string) artifactResult {
	t.Helper()
	config := artifactConfiguration(t, repositoryRoot, artifactRoot)
	if err := run(config); err != nil {
		t.Fatalf("run: %v", err)
	}
	return artifactResult{zipPath: config.zipPath, extractPath: config.extractPath}
}

func artifactConfiguration(t *testing.T, repositoryRoot, artifactRoot string) configuration {
	t.Helper()
	return configuration{
		repositoryRoot: repositoryRoot,
		commitSHA:      repositoryHead(t, repositoryRoot),
		stageDirectory: filepath.Join(artifactRoot, "stage"),
		zipPath:        filepath.Join(artifactRoot, "archive", "core.zip"),
		extractPath:    filepath.Join(artifactRoot, "extract"),
		version:        testModuleVersion,
	}
}

func newValidRepository(t *testing.T) string {
	t.Helper()
	repositoryRoot := t.TempDir()
	writeValidCoreTree(t, filepath.Join(repositoryRoot, "core"))
	runGit(t, repositoryRoot, "init", "--quiet")
	runGit(t, repositoryRoot, "add", "--force", "--all", "--", "core")
	runGit(t, repositoryRoot, "-c", "user.name=WindShare release test", "-c", "user.email=release-test@invalid.example", "commit", "--quiet", "-m", "candidate")
	return repositoryRoot
}

func newValidStage(t *testing.T) string {
	t.Helper()
	stageDirectory := t.TempDir()
	writeValidCoreTree(t, stageDirectory)
	return stageDirectory
}

func writeValidCoreTree(t *testing.T, coreDirectory string) {
	t.Helper()
	topLevel := map[string]string{
		".testcoverage.yml": "threshold:\n  total: 90\n",
		"go.mod":            "module " + modulePath + "\n\ngo 1.26.5\n",
		"go.sum":            "example.com/dependency v0.0.0 h1:fixture\n",
		"README.md":         "# WindShare Core\n",
		"LICENSE":           "Apache License\nVersion 2.0\n",
		"NOTICE":            "WindShare\n",
	}
	for name, content := range topLevel {
		writeFile(t, filepath.Join(coreDirectory, name), content)
	}
	for directory := range allowedTopLevelDirectories {
		writeFile(t, filepath.Join(coreDirectory, directory, "fixture.txt"), "fixture for "+directory+"\n")
	}
	writeFile(t, filepath.Join(coreDirectory, "testvectors", "README.md"), "# Test vectors\n")
	writeFile(t, filepath.Join(coreDirectory, "testvectors", "inventory.txt"), "vector.json\n")
	writeFile(t, filepath.Join(coreDirectory, "testvectors", "vector.json"), "{}\n")
}

func committedOrFatal(t *testing.T, repositoryRoot string) []committedFile {
	t.Helper()
	files, err := committedCoreFiles(repositoryRoot, repositoryHead(t, repositoryRoot))
	if err != nil {
		t.Fatalf("committedCoreFiles: %v", err)
	}
	return files
}

func collectRegularFiles(t *testing.T, root string) []string {
	t.Helper()
	var files []string
	err := filepath.WalkDir(root, func(filePath string, entry os.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if entry.IsDir() {
			return nil
		}
		info, err := entry.Info()
		if err != nil {
			return err
		}
		if !info.Mode().IsRegular() {
			return nil
		}
		relativePath, err := filepath.Rel(root, filePath)
		if err != nil {
			return err
		}
		files = append(files, filepath.ToSlash(relativePath))
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	sort.Strings(files)
	return files
}

func treeSnapshot(t *testing.T, root string) map[string]string {
	t.Helper()
	snapshot := make(map[string]string)
	for _, filePath := range collectRegularFiles(t, root) {
		snapshot[filePath] = readFile(t, filepath.Join(root, filepath.FromSlash(filePath)))
	}
	return snapshot
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
	return string(mustReadBytes(t, filePath))
}

func mustReadBytes(t *testing.T, filePath string) []byte {
	t.Helper()
	content, err := os.ReadFile(filePath)
	if err != nil {
		t.Fatal(err)
	}
	return content
}

func runGit(t *testing.T, repositoryRoot string, arguments ...string) {
	t.Helper()
	command := exec.Command("git", append([]string{"-C", repositoryRoot}, arguments...)...)
	command.Env = isolatedGitEnvironment()
	output, err := command.CombinedOutput()
	if err != nil {
		t.Fatalf("git %s: %v\n%s", strings.Join(arguments, " "), err, output)
	}
}

func gitOutputForTest(t *testing.T, repositoryRoot string, arguments ...string) string {
	t.Helper()
	command := exec.Command("git", append([]string{"-C", repositoryRoot}, arguments...)...)
	command.Env = isolatedGitEnvironment()
	output, err := command.CombinedOutput()
	if err != nil {
		t.Fatalf("git %s: %v\n%s", strings.Join(arguments, " "), err, output)
	}
	return string(output)
}

func repositoryHead(t *testing.T, repositoryRoot string) string {
	t.Helper()
	commitSHA := strings.TrimSpace(gitOutputForTest(t, repositoryRoot, "rev-parse", "HEAD"))
	if !isLowerHexCommitSHA(commitSHA) {
		t.Fatalf("repository HEAD is not an exact SHA: %q", commitSHA)
	}
	return commitSHA
}

func commitWorktree(t *testing.T, repositoryRoot, message string) {
	t.Helper()
	runGit(t, repositoryRoot, "add", "--all", "--", "core")
	runGit(t, repositoryRoot, "-c", "user.name=WindShare release test", "-c", "user.email=release-test@invalid.example", "commit", "--quiet", "-m", message)
}

func selectCommittedFiles(t *testing.T, files []committedFile, paths ...string) []committedFile {
	t.Helper()
	byPath := make(map[string]committedFile, len(files))
	for _, file := range files {
		byPath[file.relativePath] = file
	}
	selected := make([]committedFile, 0, len(paths))
	for _, filePath := range paths {
		file, found := byPath[filePath]
		if !found {
			t.Fatalf("committed projection is missing %s", filePath)
		}
		selected = append(selected, file)
	}
	return selected
}

func removeCommittedPath(files []committedFile, target string) []committedFile {
	result := make([]committedFile, 0, len(files))
	for _, file := range files {
		if file.relativePath != target {
			result = append(result, file)
		}
	}
	return result
}

func removeCommittedPrefix(files []committedFile, prefix string) []committedFile {
	result := make([]committedFile, 0, len(files))
	for _, file := range files {
		if !strings.HasPrefix(file.relativePath, prefix) {
			result = append(result, file)
		}
	}
	return result
}

func requireContainsPath(t *testing.T, files []string, target string, want bool) {
	t.Helper()
	index := sort.SearchStrings(files, target)
	found := index < len(files) && files[index] == target
	if found != want {
		t.Fatalf("projection contains %q = %v, want %v; files=%v", target, found, want, files)
	}
}

func requireNotExist(t *testing.T, filePath string) {
	t.Helper()
	if _, err := os.Stat(filePath); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("%s unexpectedly exists: %v", filePath, err)
	}
}

func requireErrorContains(t *testing.T, err error, want string) {
	t.Helper()
	if err == nil {
		t.Fatalf("error = nil, want substring %q", want)
	}
	if !strings.Contains(err.Error(), want) {
		t.Fatalf("error = %q, want substring %q", err, want)
	}
}

func treeRecords(records ...string) []byte {
	return append([]byte(strings.Join(records, "\x00")), 0)
}
