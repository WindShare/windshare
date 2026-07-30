package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/windshare/windshare/internal/testicetopology"
)

type resolverFunc func(context.Context, testicetopology.Profile) (testicetopology.Resolution, error)

type failingWriter struct{}

func (failingWriter) Write([]byte) (int, error) {
	return 0, errors.New("synthetic writer failure")
}

type writerFunc func([]byte) (int, error)

func (function writerFunc) Write(encoded []byte) (int, error) {
	return function(encoded)
}

func (function resolverFunc) Resolve(
	ctx context.Context,
	profile testicetopology.Profile,
) (testicetopology.Resolution, error) {
	return function(ctx, profile)
}

func TestExecuteMaterializesCanonicalCurrentMachineAuthority(t *testing.T) {
	profilePath, profile, resolution := loadFixturePair(t)
	outputPath := filepath.Join(t.TempDir(), "resolution.json")
	var stdout bytes.Buffer
	err := execute(
		context.Background(),
		[]string{"--profile", profilePath, "--output", outputPath},
		resolverFunc(func(context.Context, testicetopology.Profile) (testicetopology.Resolution, error) {
			return resolution, nil
		}),
		&stdout,
	)
	if err != nil {
		t.Fatalf("execute: %v", err)
	}
	profileSHA256, err := profile.SHA256()
	if err != nil {
		t.Fatalf("profile digest: %v", err)
	}
	expected, err := resolution.CanonicalJSON(profile, profileSHA256)
	if err != nil {
		t.Fatalf("canonical resolution: %v", err)
	}
	actual, err := os.ReadFile(outputPath)
	if err != nil {
		t.Fatalf("read materialized resolution: %v", err)
	}
	if !bytes.Equal(actual, expected) {
		t.Fatal("materialized resolution is not the shared canonical encoding")
	}
	var record materializationRecord
	if err := json.Unmarshal(stdout.Bytes(), &record); err != nil {
		t.Fatalf("decode materialization record: %v", err)
	}
	if record.Outcome != "materialized" || record.ProfilePath != profilePath || record.ResolutionPath != outputPath {
		t.Fatalf("unexpected materialization record: %+v", record)
	}
	if record.TopologyProfileSHA256 != profileSHA256 {
		t.Fatalf("profile digest = %q, want %q", record.TopologyProfileSHA256, profileSHA256)
	}
	expectedResolutionSHA256, err := resolution.SHA256(profile, profileSHA256)
	if err != nil {
		t.Fatalf("resolution digest: %v", err)
	}
	if record.TopologyResolutionSHA256 != expectedResolutionSHA256 {
		t.Fatalf("resolution digest = %q, want %q", record.TopologyResolutionSHA256, expectedResolutionSHA256)
	}
}

func TestExecuteFailsClosedBeforePublishingResolution(t *testing.T) {
	profilePath, _, _ := loadFixturePair(t)
	outputPath := filepath.Join(t.TempDir(), "resolution.json")
	err := execute(
		context.Background(),
		[]string{"--profile", profilePath, "--output", outputPath},
		resolverFunc(func(context.Context, testicetopology.Profile) (testicetopology.Resolution, error) {
			return testicetopology.Resolution{}, errors.New("synthetic resolver failure")
		}),
		&bytes.Buffer{},
	)
	if err == nil {
		t.Fatal("execute unexpectedly succeeded")
	}
	if _, statErr := os.Stat(outputPath); !errors.Is(statErr, os.ErrNotExist) {
		t.Fatalf("failed preflight published an output: %v", statErr)
	}
}

func TestExecuteRejectsWhitespaceNormalizedProfileAuthority(t *testing.T) {
	profilePath, profile, resolution := loadFixturePair(t)
	canonical, err := profile.CanonicalJSON()
	if err != nil {
		t.Fatalf("canonical profile: %v", err)
	}
	nonCanonicalPath := filepath.Join(t.TempDir(), "profile.json")
	if err := os.WriteFile(nonCanonicalPath, append(canonical, '\n'), 0o600); err != nil {
		t.Fatalf("write non-canonical profile: %v", err)
	}
	outputPath := filepath.Join(t.TempDir(), "resolution.json")
	err = execute(
		context.Background(),
		[]string{"--profile", nonCanonicalPath, "--output", outputPath},
		resolverFunc(func(context.Context, testicetopology.Profile) (testicetopology.Resolution, error) {
			return resolution, nil
		}),
		&bytes.Buffer{},
	)
	if err == nil || !strings.Contains(err.Error(), "exact canonical encoding") {
		t.Fatalf("execute error = %v, want exact canonical byte rejection", err)
	}
	if _, statErr := os.Stat(outputPath); !errors.Is(statErr, os.ErrNotExist) {
		t.Fatalf("non-canonical profile published an output: %v", statErr)
	}
	if profilePath == nonCanonicalPath {
		t.Fatal("test did not create an independent profile authority")
	}
}

func TestLoadCanonicalProfileRejectsOversizedInput(t *testing.T) {
	profilePath := filepath.Join(t.TempDir(), "profile.json")
	oversized := bytes.Repeat([]byte{'x'}, testicetopology.MaximumFileBytes+1)
	if err := os.WriteFile(profilePath, oversized, 0o600); err != nil {
		t.Fatalf("write oversized profile: %v", err)
	}
	if _, err := loadCanonicalProfile(profilePath); err == nil || !strings.Contains(err.Error(), "frozen byte limit") {
		t.Fatalf("load canonical profile error = %v, want frozen byte limit rejection", err)
	}
}

func TestExecuteRefusesStaleOrAmbiguousPaths(t *testing.T) {
	profilePath, _, resolution := loadFixturePair(t)
	outputPath := filepath.Join(t.TempDir(), "resolution.json")
	if err := os.WriteFile(outputPath, []byte("stale"), 0o600); err != nil {
		t.Fatalf("seed stale output: %v", err)
	}
	resolver := resolverFunc(func(context.Context, testicetopology.Profile) (testicetopology.Resolution, error) {
		return resolution, nil
	})
	if err := execute(
		context.Background(),
		[]string{"--profile", profilePath, "--output", outputPath},
		resolver,
		&bytes.Buffer{},
	); err == nil {
		t.Fatal("execute replaced a stale resolution authority")
	}
	actual, err := os.ReadFile(outputPath)
	if err != nil {
		t.Fatalf("read stale output: %v", err)
	}
	if string(actual) != "stale" {
		t.Fatalf("stale output was mutated: %q", actual)
	}
	if err := execute(
		context.Background(),
		[]string{"--profile", "relative-profile.json", "--output", outputPath},
		resolver,
		&bytes.Buffer{},
	); err == nil {
		t.Fatal("execute accepted a relative profile authority")
	}
}

func TestExecuteHelpPublishesTheStablePreflightContract(t *testing.T) {
	var stdout bytes.Buffer
	if err := execute(context.Background(), []string{"--help"}, nil, &stdout); err != nil {
		t.Fatalf("execute help: %v", err)
	}
	if stdout.String() != topologyResolutionUsage+"\n" {
		t.Fatalf("help = %q, want %q", stdout.String(), topologyResolutionUsage+"\n")
	}
}

func TestExecuteRejectsMissingDependenciesAndInvalidProfiles(t *testing.T) {
	profilePath, _, resolution := loadFixturePair(t)
	outputPath := filepath.Join(t.TempDir(), "resolution.json")
	args := []string{"--profile", profilePath, "--output", outputPath}

	if err := execute(context.Background(), args, nil, &bytes.Buffer{}); err == nil {
		t.Fatal("execute accepted a nil topology resolver")
	}
	if err := execute(
		context.Background(),
		args,
		resolverFunc(func(context.Context, testicetopology.Profile) (testicetopology.Resolution, error) {
			return resolution, nil
		}),
		nil,
	); err == nil {
		t.Fatal("execute accepted a nil stdout writer")
	}

	missingProfilePath := filepath.Join(t.TempDir(), "missing-profile.json")
	resolver := resolverFunc(func(context.Context, testicetopology.Profile) (testicetopology.Resolution, error) {
		return resolution, nil
	})
	if err := execute(
		context.Background(),
		[]string{"--profile", missingProfilePath, "--output", outputPath},
		resolver,
		&bytes.Buffer{},
	); err == nil {
		t.Fatal("execute accepted a missing topology profile")
	}
}

func TestExecuteDoesNotPublishWhenMaterializationRecordWriteFails(t *testing.T) {
	profilePath, _, resolution := loadFixturePair(t)
	outputDirectory := t.TempDir()
	outputPath := filepath.Join(outputDirectory, "resolution.json")
	err := execute(
		context.Background(),
		[]string{"--profile", profilePath, "--output", outputPath},
		resolverFunc(func(context.Context, testicetopology.Profile) (testicetopology.Resolution, error) {
			return resolution, nil
		}),
		failingWriter{},
	)
	if err == nil {
		t.Fatal("execute hid the materialization record failure")
	}
	if _, statErr := os.Stat(outputPath); !errors.Is(statErr, os.ErrNotExist) {
		t.Fatalf("reporting failure published an immutable authority: %v", statErr)
	}
	entries, readErr := os.ReadDir(outputDirectory)
	if readErr != nil {
		t.Fatalf("read output directory: %v", readErr)
	}
	if len(entries) != 0 {
		t.Fatalf("reporting failure left staged files behind: %+v", entries)
	}
}

func TestExecuteReportsBeforeCreatingTheStagedPublication(t *testing.T) {
	profilePath, _, resolution := loadFixturePair(t)
	outputDirectory := t.TempDir()
	outputPath := filepath.Join(outputDirectory, "resolution.json")
	recordObserved := false
	err := execute(
		context.Background(),
		[]string{"--profile", profilePath, "--output", outputPath},
		resolverFunc(func(context.Context, testicetopology.Profile) (testicetopology.Resolution, error) {
			return resolution, nil
		}),
		writerFunc(func(encoded []byte) (int, error) {
			entries, readErr := os.ReadDir(outputDirectory)
			if readErr != nil {
				return 0, readErr
			}
			if len(entries) != 0 {
				return 0, fmt.Errorf("materialization record observed staged paths: %+v", entries)
			}
			recordObserved = true
			return len(encoded), nil
		}),
	)
	if err != nil {
		t.Fatalf("execute: %v", err)
	}
	if !recordObserved {
		t.Fatal("materialization record writer was not invoked")
	}
}

func TestParseCommandOptionsRejectsAmbiguousInvocation(t *testing.T) {
	profilePath, _, _ := loadFixturePair(t)
	outputPath := filepath.Join(t.TempDir(), "resolution.json")
	testCases := []struct {
		name string
		args []string
	}{
		{name: "unknown option", args: []string{"--unknown"}},
		{name: "positional argument", args: []string{"--profile", profilePath, "--output", outputPath, "extra"}},
		{name: "missing profile", args: []string{"--output", outputPath}},
		{name: "missing output", args: []string{"--profile", profilePath}},
		{name: "same authority", args: []string{"--profile", profilePath, "--output", profilePath}},
	}
	for _, testCase := range testCases {
		t.Run(testCase.name, func(t *testing.T) {
			if _, err := parseCommandOptions(testCase.args); err == nil {
				t.Fatalf("parseCommandOptions(%q) unexpectedly succeeded", testCase.args)
			}
		})
	}
}

func TestExecuteReportsHelpWriterFailure(t *testing.T) {
	if err := execute(context.Background(), []string{"--help"}, nil, failingWriter{}); err == nil {
		t.Fatal("execute hid the help writer failure")
	}
}

func TestWriteNewAtomicHasSingleWinner(t *testing.T) {
	outputPath := filepath.Join(t.TempDir(), "resolution.json")
	start := make(chan struct{})
	results := make(chan error, 2)
	for _, content := range [][]byte{[]byte("first"), []byte("second")} {
		content := content
		go func() {
			<-start
			results <- writeNewAtomic(outputPath, content)
		}()
	}
	close(start)

	successCount := 0
	for range 2 {
		if err := <-results; err == nil {
			successCount++
		}
	}
	if successCount != 1 {
		t.Fatalf("successful atomic publishers = %d, want 1", successCount)
	}
	actual, err := os.ReadFile(outputPath)
	if err != nil {
		t.Fatalf("read winner: %v", err)
	}
	if string(actual) != "first" && string(actual) != "second" {
		t.Fatalf("winner contains unexpected bytes: %q", actual)
	}
}

func TestWriteNewAtomicRejectsNonDirectoryParent(t *testing.T) {
	parentPath := filepath.Join(t.TempDir(), "not-a-directory")
	if err := os.WriteFile(parentPath, []byte("occupied"), 0o600); err != nil {
		t.Fatalf("seed non-directory parent: %v", err)
	}
	if err := writeNewAtomic(filepath.Join(parentPath, "resolution.json"), []byte("resolution")); err == nil {
		t.Fatal("writeNewAtomic accepted a non-directory parent")
	}
}

func loadFixturePair(
	t *testing.T,
) (string, testicetopology.Profile, testicetopology.Resolution) {
	t.Helper()
	profilePath, err := filepath.Abs(filepath.Join(
		"..", "..", "..", "..", "testdata", "test-ice-topology",
		"pr-same-host-kernel-route-ipv4.json",
	))
	if err != nil {
		t.Fatalf("absolute profile path: %v", err)
	}
	resolutionPath := filepath.Join(
		filepath.Dir(profilePath),
		"pr-same-host-kernel-route-ipv4-resolution.json",
	)
	profile, err := testicetopology.Load(profilePath)
	if err != nil {
		t.Fatalf("load profile fixture: %v", err)
	}
	profileSHA256, err := profile.SHA256()
	if err != nil {
		t.Fatalf("profile fixture digest: %v", err)
	}
	resolution, err := testicetopology.LoadResolution(resolutionPath, profile, profileSHA256)
	if err != nil {
		t.Fatalf("load resolution fixture: %v", err)
	}
	return profilePath, profile, resolution
}
