package main

import (
	"context"
	"encoding/json"
	"fmt"
	"os"

	pion "github.com/pion/webrtc/v4"
	"github.com/windshare/windshare/internal/testicetopology"
)

const (
	topologyProfilePathEnv      = "WINDSHARE_TEST_ICE_TOPOLOGY_PROFILE"
	topologyResolutionPathEnv   = "WINDSHARE_TEST_ICE_TOPOLOGY_RESOLUTION"
	expectedProfileDigestEnv    = "WINDSHARE_TEST_ICE_TOPOLOGY_PROFILE_SHA256"
	expectedResolutionDigestEnv = "WINDSHARE_TEST_ICE_TOPOLOGY_RESOLUTION_SHA256"
)

type topologyResolver interface {
	Resolve(context.Context, testicetopology.Profile) (testicetopology.Resolution, error)
}

type serializedTopologyLock struct {
	Profile          json.RawMessage `json:"profile"`
	Resolution       json.RawMessage `json:"resolution"`
	ProfileSHA256    string          `json:"profileSha256"`
	ResolutionSHA256 string          `json:"resolutionSha256"`
}

type topologyRuntime struct {
	profile          testicetopology.Profile
	resolution       testicetopology.Resolution
	profileSHA256    string
	resolutionSHA256 string
	public           serializedTopologyLock
}

func loadTopologyRuntime(ctx context.Context) (*topologyRuntime, error) {
	resolver := testicetopology.NewStandardResolver()
	return loadTopologyRuntimeWith(ctx, os.Getenv, resolver)
}

func loadTopologyRuntimeWith(
	ctx context.Context,
	getenv func(string) string,
	resolver topologyResolver,
) (*topologyRuntime, error) {
	if getenv == nil || resolver == nil {
		return nil, fmt.Errorf("topology environment and resolver are required")
	}
	profilePath := getenv(topologyProfilePathEnv)
	resolutionPath := getenv(topologyResolutionPathEnv)
	expectedProfileDigest := getenv(expectedProfileDigestEnv)
	expectedResolutionDigest := getenv(expectedResolutionDigestEnv)
	if profilePath == "" {
		if resolutionPath != "" || expectedProfileDigest != "" || expectedResolutionDigest != "" {
			return nil, fmt.Errorf("%s is required when any topology lock input is configured", topologyProfilePathEnv)
		}
		return nil, nil
	}
	profile, err := testicetopology.Load(profilePath)
	if err != nil {
		return nil, fmt.Errorf("load browser/Pion topology profile: %w", err)
	}
	profileDigest, err := profile.SHA256()
	if err != nil {
		return nil, fmt.Errorf("digest browser/Pion topology profile: %w", err)
	}
	if expectedProfileDigest != "" && expectedProfileDigest != profileDigest {
		return nil, fmt.Errorf("browser/Pion topology profile digest differs from %s", expectedProfileDigestEnv)
	}

	resolved, err := resolver.Resolve(ctx, profile)
	if err != nil {
		return nil, fmt.Errorf("resolve browser/Pion topology: %w", err)
	}
	resolution := resolved
	if resolutionPath != "" {
		resolution, err = testicetopology.LoadResolution(resolutionPath, profile, profileDigest)
		if err != nil {
			return nil, fmt.Errorf("load browser/Pion topology resolution: %w", err)
		}
		if err := requireSameResolution(profile, profileDigest, resolution, resolved); err != nil {
			return nil, err
		}
	}
	resolutionDigest, err := resolution.SHA256(profile, profileDigest)
	if err != nil {
		return nil, fmt.Errorf("digest browser/Pion topology resolution: %w", err)
	}
	if expectedResolutionDigest != "" && expectedResolutionDigest != resolutionDigest {
		return nil, fmt.Errorf("browser/Pion topology resolution digest differs from %s", expectedResolutionDigestEnv)
	}
	profileJSON, err := profile.CanonicalJSON()
	if err != nil {
		return nil, fmt.Errorf("serialize browser/Pion topology profile: %w", err)
	}
	resolutionJSON, err := resolution.CanonicalJSON(profile, profileDigest)
	if err != nil {
		return nil, fmt.Errorf("serialize browser/Pion topology resolution: %w", err)
	}
	return &topologyRuntime{
		profile:          profile,
		resolution:       resolution,
		profileSHA256:    profileDigest,
		resolutionSHA256: resolutionDigest,
		public: serializedTopologyLock{
			Profile:          profileJSON,
			Resolution:       resolutionJSON,
			ProfileSHA256:    profileDigest,
			ResolutionSHA256: resolutionDigest,
		},
	}, nil
}

func requireSameResolution(
	profile testicetopology.Profile,
	profileDigest string,
	serialized testicetopology.Resolution,
	current testicetopology.Resolution,
) error {
	serializedDigest, err := serialized.SHA256(profile, profileDigest)
	if err != nil {
		return fmt.Errorf("digest serialized browser/Pion topology resolution: %w", err)
	}
	currentDigest, err := current.SHA256(profile, profileDigest)
	if err != nil {
		return fmt.Errorf("digest current browser/Pion topology resolution: %w", err)
	}
	if serializedDigest != currentDigest {
		return fmt.Errorf("serialized browser/Pion topology resolution does not match the current kernel route")
	}
	return nil
}

func (runtime *topologyRuntime) newPeerConnection() (*pion.PeerConnection, error) {
	setting, err := testicetopology.PionSettingEngine(runtime.profile, runtime.resolution)
	if err != nil {
		return nil, fmt.Errorf("configure topology-bound Pion setting engine: %w", err)
	}
	configuration, err := testicetopology.PionConfiguration(runtime.profile)
	if err != nil {
		return nil, fmt.Errorf("configure topology-bound Pion peer: %w", err)
	}
	return pion.NewAPI(pion.WithSettingEngine(setting)).NewPeerConnection(configuration)
}
