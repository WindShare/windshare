package main

import (
	"bytes"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/windshare/windshare/cmd/windshare/internal/cli"
	"github.com/windshare/windshare/internal/testicetopology"
)

type topologyFixtureLock struct {
	profilePath      string
	resolutionPath   string
	profileSHA256    string
	resolutionSHA256 string
	profile          testicetopology.Profile
	resolution       testicetopology.Resolution
}

func TestParseE2EArgumentsRequiresCompleteCanonicalLock(t *testing.T) {
	lock := loadTopologyFixtureLock(t)
	evidencePath := filepath.Join(t.TempDir(), "attempts.jsonl")
	valid := lockedE2EArguments(lock, evidencePath, "share", "folder", "--relay", "ws://relay")
	parsed, err := parseE2EArguments(valid)
	if err != nil {
		t.Fatalf("parse valid arguments: %v", err)
	}
	if parsed.profilePath != lock.profilePath || parsed.resolutionPath != lock.resolutionPath ||
		parsed.expectedProfileSHA256 != lock.profileSHA256 ||
		parsed.expectedResolutionSHA256 != lock.resolutionSHA256 ||
		parsed.evidencePath != evidencePath || strings.Join(parsed.command, " ") != "share folder --relay ws://relay" {
		t.Fatalf("parsed arguments = %#v", parsed)
	}

	for name, args := range map[string][]string{
		"missing lock":         {"share", "file"},
		"relative path":        replaceArgument(valid, topologyProfileOption, "profile.json"),
		"uppercase digest":     replaceArgument(valid, topologyProfileDigestOption, strings.ToUpper(lock.profileSHA256)),
		"short digest":         replaceArgument(valid, topologyResolutionDigestOption, "abcd"),
		"shared output":        replaceArgument(valid, senderEvidenceOption, lock.resolutionPath),
		"unknown option":       append([]string{"--unknown", "value"}, valid...),
		"duplicate profile":    append([]string{topologyProfileOption, lock.profilePath}, valid...),
		"missing command":      valid[:len(valid)-4],
		"missing option value": {topologyProfileOption},
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := parseE2EArguments(args); err == nil {
				t.Fatalf("parseE2EArguments(%q) succeeded", args)
			}
		})
	}
}

func TestPrepareE2ERejectsMalformedFrozenArtifactsBeforeOpeningEvidence(t *testing.T) {
	lock := loadTopologyFixtureLock(t)
	unknownProfile := strings.Replace(
		string(readTopologyFixture(t, "pr-same-host-kernel-route-ipv4.json")),
		testicetopology.PRTopologyID,
		"unknown-topology",
		1,
	)
	for name, mutation := range map[string]struct {
		profile    string
		resolution string
	}{
		"malformed profile":    {profile: "{"},
		"unknown profile":      {profile: unknownProfile},
		"malformed resolution": {resolution: "{"},
	} {
		t.Run(name, func(t *testing.T) {
			directory := t.TempDir()
			profilePath := lock.profilePath
			resolutionPath := lock.resolutionPath
			if mutation.profile != "" {
				profilePath = filepath.Join(directory, "profile.json")
				if err := os.WriteFile(profilePath, []byte(mutation.profile), 0o600); err != nil {
					t.Fatal(err)
				}
			}
			if mutation.resolution != "" {
				resolutionPath = filepath.Join(directory, "resolution.json")
				if err := os.WriteFile(resolutionPath, []byte(mutation.resolution), 0o600); err != nil {
					t.Fatal(err)
				}
			}
			opened := false
			_, err := prepareE2E(lockedE2EArguments(topologyFixtureLock{
				profilePath: profilePath, resolutionPath: resolutionPath,
				profileSHA256: lock.profileSHA256, resolutionSHA256: lock.resolutionSHA256,
			}, filepath.Join(directory, "attempts.jsonl"), "share", "folder"), e2eDependencies{
				loadProfile:    testicetopology.Load,
				loadResolution: testicetopology.LoadResolution,
				openEvidence: func(string) (io.WriteCloser, error) {
					opened = true
					return &trackingWriteCloser{}, nil
				},
			})
			if err == nil || opened {
				t.Fatalf("prepare invalid frozen artifacts: err=%v opened=%t", err, opened)
			}
		})
	}
}

func TestPrepareE2EVerifiesBothPublishedDigestsBeforeOpeningEvidence(t *testing.T) {
	lock := loadTopologyFixtureLock(t)
	for _, test := range []struct {
		name               string
		profileSHA256      string
		resolutionSHA256   string
		wantResolutionLoad bool
	}{
		{name: "profile", profileSHA256: strings.Repeat("f", sha256TextBytes), resolutionSHA256: lock.resolutionSHA256},
		{name: "resolution", profileSHA256: lock.profileSHA256, resolutionSHA256: strings.Repeat("f", sha256TextBytes), wantResolutionLoad: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			opened := false
			loadedResolution := false
			_, err := prepareE2E(lockedE2EArguments(topologyFixtureLock{
				profilePath: lock.profilePath, resolutionPath: lock.resolutionPath,
				profileSHA256: test.profileSHA256, resolutionSHA256: test.resolutionSHA256,
			}, filepath.Join(t.TempDir(), "attempts.jsonl"), "share", "folder"), e2eDependencies{
				loadProfile: func(string) (testicetopology.Profile, error) { return lock.profile, nil },
				loadResolution: func(string, testicetopology.Profile, string) (testicetopology.Resolution, error) {
					loadedResolution = true
					return lock.resolution, nil
				},
				openEvidence: func(string) (io.WriteCloser, error) {
					opened = true
					return &trackingWriteCloser{}, nil
				},
			})
			if err == nil || opened || loadedResolution != test.wantResolutionLoad {
				t.Fatalf("digest mismatch: err=%v opened=%t loadedResolution=%t", err, opened, loadedResolution)
			}
		})
	}
}

func TestPrepareE2EBindsProviderOnlyToPublishedResolution(t *testing.T) {
	lock := loadTopologyFixtureLock(t)
	evidencePath := filepath.Join(t.TempDir(), "attempts.jsonl")
	evidence := &trackingWriteCloser{}
	prepared, err := prepareE2E(
		lockedE2EArguments(lock, evidencePath, "share", "folder"),
		e2eDependencies{
			loadProfile: func(path string) (testicetopology.Profile, error) {
				if path != lock.profilePath {
					t.Fatalf("profile path = %q", path)
				}
				return lock.profile, nil
			},
			loadResolution: func(path string, profile testicetopology.Profile, expectedProfileSHA256 string) (testicetopology.Resolution, error) {
				if path != lock.resolutionPath || profile.TopologyID != lock.profile.TopologyID ||
					expectedProfileSHA256 != lock.profileSHA256 {
					t.Fatalf("resolution lock = %q %#v %q", path, profile, expectedProfileSHA256)
				}
				return lock.resolution, nil
			},
			openEvidence: func(path string) (io.WriteCloser, error) {
				if path != evidencePath {
					t.Fatalf("evidence path = %q", path)
				}
				return evidence, nil
			},
		},
	)
	if err != nil {
		t.Fatalf("prepare e2e: %v", err)
	}
	if len(prepared.provider.configuration.ICEServers) != 0 || prepared.provider.configuration.ICEServers == nil {
		t.Fatalf("provider ICE servers = %#v", prepared.provider.configuration.ICEServers)
	}
	if _, err := prepared.provider.NewSenderPeerFactory(cli.SenderPeerFactoryOptions{}); err != nil {
		t.Fatalf("construct sender peer factory: %v", err)
	}
	if strings.Join(prepared.command, " ") != "share folder" || prepared.evidence != evidence {
		t.Fatalf("prepared process = %#v", prepared)
	}
}

func TestPrepareE2ERequiresFreshEvidenceAndPreservesFrozenArtifacts(t *testing.T) {
	for _, test := range []struct {
		name      string
		aliasPath func(topologyFixtureLock) string
		prepare   func(string, topologyFixtureLock) error
	}{
		{
			name: "pre-existing file",
			aliasPath: func(lock topologyFixtureLock) string {
				return filepath.Join(filepath.Dir(lock.profilePath), "existing-attempts.jsonl")
			},
			prepare: func(path string, _ topologyFixtureLock) error {
				return os.WriteFile(path, []byte("existing evidence"), 0o600)
			},
		},
		{
			name: "profile hard link",
			aliasPath: func(lock topologyFixtureLock) string {
				return filepath.Join(filepath.Dir(lock.profilePath), "profile-alias.jsonl")
			},
			prepare: func(path string, lock topologyFixtureLock) error {
				return os.Link(lock.profilePath, path)
			},
		},
		{
			name: "resolution symbolic link",
			aliasPath: func(lock topologyFixtureLock) string {
				return filepath.Join(filepath.Dir(lock.profilePath), "resolution-alias.jsonl")
			},
			prepare: func(path string, lock topologyFixtureLock) error {
				return os.Symlink(lock.resolutionPath, path)
			},
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			lock := copyTopologyFixtureLock(t)
			profileBefore, err := os.ReadFile(lock.profilePath)
			if err != nil {
				t.Fatal(err)
			}
			resolutionBefore, err := os.ReadFile(lock.resolutionPath)
			if err != nil {
				t.Fatal(err)
			}
			evidencePath := test.aliasPath(lock)
			if err := test.prepare(evidencePath, lock); err != nil {
				if test.name == "resolution symbolic link" {
					t.Skipf("symbolic links are unavailable: %v", err)
				}
				t.Fatal(err)
			}

			if prepared, err := prepareE2E(
				lockedE2EArguments(lock, evidencePath, "help"),
				defaultE2EDependencies(),
			); err == nil {
				_ = prepared.evidence.Close()
				t.Fatal("pre-existing evidence artifact was accepted")
			}
			profileAfter, err := os.ReadFile(lock.profilePath)
			if err != nil {
				t.Fatal(err)
			}
			resolutionAfter, err := os.ReadFile(lock.resolutionPath)
			if err != nil {
				t.Fatal(err)
			}
			if !bytes.Equal(profileAfter, profileBefore) || !bytes.Equal(resolutionAfter, resolutionBefore) {
				t.Fatal("rejected evidence alias modified a frozen topology artifact")
			}
		})
	}
}

func TestRunE2EForwardsOnlyProductionCLIArgumentsAndClosesEvidence(t *testing.T) {
	lock := loadTopologyFixtureLock(t)
	evidence := &trackingWriteCloser{}
	var received []string
	code := runE2E(
		lockedE2EArguments(lock, filepath.Join(t.TempDir(), "attempts.jsonl"), "share", "folder"),
		io.Discard,
		e2eDependencies{
			loadProfile: func(string) (testicetopology.Profile, error) { return lock.profile, nil },
			loadResolution: func(string, testicetopology.Profile, string) (testicetopology.Resolution, error) {
				return lock.resolution, nil
			},
			openEvidence: func(string) (io.WriteCloser, error) { return evidence, nil },
		},
		func(args []string, config cli.ProcessConfig) int {
			received = append([]string(nil), args...)
			if config.SenderPeerFactories == nil || config.SenderPeerEvidence != evidence {
				t.Fatalf("process config = %#v", config)
			}
			return cli.ExitNetwork
		},
	)
	if code != cli.ExitNetwork || strings.Join(received, " ") != "share folder" || !evidence.closed {
		t.Fatalf("run result: code=%d args=%q closed=%t", code, received, evidence.closed)
	}
}

func TestRunE2EReportsPreparationAndCloseFailures(t *testing.T) {
	var stderr bytes.Buffer
	if code := runE2E(
		nil,
		&stderr,
		e2eDependencies{},
		func([]string, cli.ProcessConfig) int { t.Fatal("invalid preparation reached CLI"); return 0 },
	); code != cli.ExitUsage || !strings.Contains(stderr.String(), "Usage:") {
		t.Fatalf("preparation result: code=%d stderr=%q", code, stderr.String())
	}

	lock := loadTopologyFixtureLock(t)
	stderr.Reset()
	evidence := &trackingWriteCloser{closeErr: errors.New("disk close failed")}
	code := runE2E(
		lockedE2EArguments(lock, filepath.Join(t.TempDir(), "attempts.jsonl"), "help"),
		&stderr,
		e2eDependencies{
			loadProfile: func(string) (testicetopology.Profile, error) { return lock.profile, nil },
			loadResolution: func(string, testicetopology.Profile, string) (testicetopology.Resolution, error) {
				return lock.resolution, nil
			},
			openEvidence: func(string) (io.WriteCloser, error) { return evidence, nil },
		},
		func([]string, cli.ProcessConfig) int { return cli.ExitOK },
	)
	if code != cli.ExitFailure || !strings.Contains(stderr.String(), "disk close failed") {
		t.Fatalf("close result: code=%d stderr=%q", code, stderr.String())
	}
}

type trackingWriteCloser struct {
	bytes.Buffer
	closed   bool
	closeErr error
}

func (writer *trackingWriteCloser) Close() error {
	writer.closed = true
	return writer.closeErr
}

func loadTopologyFixtureLock(t *testing.T) topologyFixtureLock {
	t.Helper()
	profilePath, err := filepath.Abs(filepath.Join("..", "..", "..", "testdata", "test-ice-topology", "pr-same-host-kernel-route-ipv4.json"))
	if err != nil {
		t.Fatalf("absolute profile fixture: %v", err)
	}
	resolutionPath := filepath.Join(filepath.Dir(profilePath), "pr-same-host-kernel-route-ipv4-resolution.json")
	profile, err := testicetopology.Load(profilePath)
	if err != nil {
		t.Fatalf("load profile fixture: %v", err)
	}
	profileSHA256, err := profile.SHA256()
	if err != nil {
		t.Fatalf("profile hash: %v", err)
	}
	resolution, err := testicetopology.LoadResolution(resolutionPath, profile, profileSHA256)
	if err != nil {
		t.Fatalf("load resolution fixture: %v", err)
	}
	resolutionSHA256, err := resolution.SHA256(profile, profileSHA256)
	if err != nil {
		t.Fatalf("resolution hash: %v", err)
	}
	return topologyFixtureLock{
		profilePath: profilePath, resolutionPath: resolutionPath,
		profileSHA256: profileSHA256, resolutionSHA256: resolutionSHA256,
		profile: profile, resolution: resolution,
	}
}

func copyTopologyFixtureLock(t *testing.T) topologyFixtureLock {
	t.Helper()
	directory := t.TempDir()
	for _, name := range []string{
		"pr-same-host-kernel-route-ipv4.json",
		"pr-same-host-kernel-route-ipv4-resolution.json",
	} {
		if err := os.WriteFile(
			filepath.Join(directory, name),
			readTopologyFixture(t, name),
			0o600,
		); err != nil {
			t.Fatal(err)
		}
	}
	profilePath := filepath.Join(directory, "pr-same-host-kernel-route-ipv4.json")
	resolutionPath := filepath.Join(directory, "pr-same-host-kernel-route-ipv4-resolution.json")
	profile, err := testicetopology.Load(profilePath)
	if err != nil {
		t.Fatal(err)
	}
	profileSHA256, err := profile.SHA256()
	if err != nil {
		t.Fatal(err)
	}
	resolution, err := testicetopology.LoadResolution(resolutionPath, profile, profileSHA256)
	if err != nil {
		t.Fatal(err)
	}
	resolutionSHA256, err := resolution.SHA256(profile, profileSHA256)
	if err != nil {
		t.Fatal(err)
	}
	return topologyFixtureLock{
		profilePath: profilePath, resolutionPath: resolutionPath,
		profileSHA256: profileSHA256, resolutionSHA256: resolutionSHA256,
		profile: profile, resolution: resolution,
	}
}

func lockedE2EArguments(lock topologyFixtureLock, evidencePath string, command ...string) []string {
	return append([]string{
		topologyProfileOption, lock.profilePath,
		topologyResolutionOption, lock.resolutionPath,
		topologyProfileDigestOption, lock.profileSHA256,
		topologyResolutionDigestOption, lock.resolutionSHA256,
		senderEvidenceOption, evidencePath,
	}, command...)
}

func replaceArgument(arguments []string, option, value string) []string {
	replaced := append([]string(nil), arguments...)
	for index := range replaced {
		if replaced[index] == option && index+1 < len(replaced) {
			replaced[index+1] = value
			return replaced
		}
	}
	return replaced
}

func readTopologyFixture(t *testing.T, name string) []byte {
	t.Helper()
	encoded, err := os.ReadFile(filepath.Join("..", "..", "..", "testdata", "test-ice-topology", name))
	if err != nil {
		t.Fatalf("read topology fixture: %v", err)
	}
	return encoded
}
