package main

import (
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"strings"

	pion "github.com/pion/webrtc/v4"
	"github.com/windshare/windshare/cmd/windshare/internal/cli"
	"github.com/windshare/windshare/connectivity/v2peer"
	"github.com/windshare/windshare/internal/testicetopology"
)

const (
	topologyProfileOption          = "--test-ice-topology"
	topologyResolutionOption       = "--test-ice-topology-resolution"
	topologyProfileDigestOption    = "--test-ice-topology-profile-sha256"
	topologyResolutionDigestOption = "--test-ice-topology-resolution-sha256"
	senderEvidenceOption           = "--sender-evidence"
	sha256TextBytes                = 64
)

type e2eArguments struct {
	profilePath              string
	resolutionPath           string
	expectedProfileSHA256    string
	expectedResolutionSHA256 string
	evidencePath             string
	command                  []string
}

type e2eDependencies struct {
	loadProfile    func(string) (testicetopology.Profile, error)
	loadResolution func(string, testicetopology.Profile, string) (testicetopology.Resolution, error)
	openEvidence   func(string) (io.WriteCloser, error)
}

type preparedE2E struct {
	command  []string
	provider *topologySenderPeerFactoryProvider
	evidence io.WriteCloser
}

func defaultE2EDependencies() e2eDependencies {
	return e2eDependencies{
		loadProfile:    testicetopology.Load,
		loadResolution: testicetopology.LoadResolution,
		openEvidence:   openEvidenceFile,
	}
}

func prepareE2E(
	args []string,
	dependencies e2eDependencies,
) (preparedE2E, error) {
	parsed, err := parseE2EArguments(args)
	if err != nil {
		return preparedE2E{}, err
	}
	if dependencies.loadProfile == nil || dependencies.loadResolution == nil || dependencies.openEvidence == nil {
		return preparedE2E{}, errors.New("test process dependencies are incomplete")
	}
	profile, err := dependencies.loadProfile(parsed.profilePath)
	if err != nil {
		return preparedE2E{}, fmt.Errorf("load test ICE topology: %w", err)
	}
	profileSHA256, err := profile.SHA256()
	if err != nil {
		return preparedE2E{}, fmt.Errorf("hash test ICE topology: %w", err)
	}
	if profileSHA256 != parsed.expectedProfileSHA256 {
		return preparedE2E{}, errors.New("test ICE topology profile differs from the A0-published digest")
	}
	resolution, err := dependencies.loadResolution(
		parsed.resolutionPath,
		profile,
		parsed.expectedProfileSHA256,
	)
	if err != nil {
		return preparedE2E{}, fmt.Errorf("load test ICE topology resolution: %w", err)
	}
	resolutionSHA256, err := resolution.SHA256(profile, parsed.expectedProfileSHA256)
	if err != nil {
		return preparedE2E{}, fmt.Errorf("hash test ICE topology resolution: %w", err)
	}
	if resolutionSHA256 != parsed.expectedResolutionSHA256 {
		return preparedE2E{}, errors.New("test ICE topology resolution differs from the A0-published digest")
	}
	provider, err := newTopologySenderPeerFactoryProvider(profile, resolution)
	if err != nil {
		return preparedE2E{}, fmt.Errorf("construct test ICE peer provider: %w", err)
	}
	evidence, err := dependencies.openEvidence(parsed.evidencePath)
	if err != nil {
		return preparedE2E{}, fmt.Errorf("open sender evidence: %w", err)
	}
	return preparedE2E{
		command: append([]string(nil), parsed.command...), provider: provider, evidence: evidence,
	}, nil
}

func parseE2EArguments(args []string) (e2eArguments, error) {
	var parsed e2eArguments
	seen := make(map[string]struct{}, 5)
	for len(args) > 0 && len(args[0]) > 2 && args[0][:2] == "--" {
		name := args[0]
		switch name {
		case topologyProfileOption, topologyResolutionOption,
			topologyProfileDigestOption, topologyResolutionDigestOption,
			senderEvidenceOption:
		default:
			return e2eArguments{}, fmt.Errorf("unknown test process option %q", name)
		}
		if _, duplicate := seen[name]; duplicate {
			return e2eArguments{}, fmt.Errorf("test process option %q was repeated", name)
		}
		if len(args) < 2 || args[1] == "" {
			return e2eArguments{}, fmt.Errorf("test process option %q requires a path", name)
		}
		seen[name] = struct{}{}
		switch name {
		case topologyProfileOption:
			parsed.profilePath = args[1]
		case topologyResolutionOption:
			parsed.resolutionPath = args[1]
		case topologyProfileDigestOption:
			parsed.expectedProfileSHA256 = args[1]
		case topologyResolutionDigestOption:
			parsed.expectedResolutionSHA256 = args[1]
		case senderEvidenceOption:
			parsed.evidencePath = args[1]
		}
		args = args[2:]
	}
	if parsed.profilePath == "" || parsed.resolutionPath == "" ||
		parsed.expectedProfileSHA256 == "" || parsed.expectedResolutionSHA256 == "" ||
		parsed.evidencePath == "" || len(args) == 0 {
		return e2eArguments{}, errors.New("topology profile, resolution, both digests, sender evidence, and command are required")
	}
	if !validSHA256(parsed.expectedProfileSHA256) || !validSHA256(parsed.expectedResolutionSHA256) {
		return e2eArguments{}, errors.New("topology digests must be 64 lowercase hexadecimal characters")
	}
	for _, target := range []struct {
		label string
		path  *string
	}{
		{label: "topology profile", path: &parsed.profilePath},
		{label: "topology resolution", path: &parsed.resolutionPath},
		{label: "sender evidence", path: &parsed.evidencePath},
	} {
		if !filepath.IsAbs(*target.path) || filepath.Clean(*target.path) != *target.path {
			return e2eArguments{}, fmt.Errorf("%s path must be absolute and canonical", target.label)
		}
	}
	if samePath(parsed.profilePath, parsed.resolutionPath) ||
		samePath(parsed.profilePath, parsed.evidencePath) ||
		samePath(parsed.resolutionPath, parsed.evidencePath) {
		return e2eArguments{}, errors.New("topology profile, resolution, and sender evidence must use distinct paths")
	}
	parsed.command = append([]string(nil), args...)
	return parsed, nil
}

func validSHA256(value string) bool {
	if len(value) != sha256TextBytes {
		return false
	}
	for _, character := range value {
		if (character < '0' || character > '9') && (character < 'a' || character > 'f') {
			return false
		}
	}
	return true
}

func samePath(left, right string) bool {
	if runtime.GOOS == "windows" {
		return strings.EqualFold(left, right)
	}
	return left == right
}

func openEvidenceFile(path string) (io.WriteCloser, error) {
	// A fresh name is part of the evidence authority: O_EXCL rejects ordinary,
	// hard-link, and symlink aliases before they can mutate either frozen lock.
	return os.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
}

type topologySenderPeerFactoryProvider struct {
	configuration pion.Configuration
	api           *pion.API
}

func newTopologySenderPeerFactoryProvider(
	profile testicetopology.Profile,
	resolution testicetopology.Resolution,
) (*topologySenderPeerFactoryProvider, error) {
	configuration, err := testicetopology.PionConfiguration(profile)
	if err != nil {
		return nil, err
	}
	setting, err := testicetopology.PionSettingEngine(profile, resolution)
	if err != nil {
		return nil, err
	}
	return &topologySenderPeerFactoryProvider{
		configuration: configuration,
		api:           pion.NewAPI(pion.WithSettingEngine(setting)),
	}, nil
}

func (provider *topologySenderPeerFactoryProvider) NewSenderPeerFactory(
	options cli.SenderPeerFactoryOptions,
) (*v2peer.Factory, error) {
	if provider == nil || provider.api == nil {
		return nil, v2peer.ErrConfig
	}
	return v2peer.NewFactory(v2peer.Config{
		Configuration: provider.configuration,
		PeerConnections: v2peer.PeerConnectionFactoryFunc(
			func(configuration pion.Configuration) (v2peer.PeerConnection, error) {
				return provider.api.NewPeerConnection(configuration)
			},
		),
		Observer: options.Observer,
		OnError:  options.OnError,
	})
}

var _ cli.SenderPeerFactoryProvider = (*topologySenderPeerFactoryProvider)(nil)
