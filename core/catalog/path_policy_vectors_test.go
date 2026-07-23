package catalog

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"testing"
)

const expectedPathPolicyUnicodeVersion = "15.0.0"

type pathPolicyVector struct {
	Version int                    `json:"version"`
	Kind    string                 `json:"kind"`
	Cases   []pathPolicyVectorCase `json:"cases"`
}

type pathPolicyVectorCase struct {
	Name           string `json:"name"`
	PolicyVersion  string `json:"policyVersion"`
	UnicodeVersion string `json:"unicodeVersion"`
	Input          string `json:"input"`
	Expected       string `json:"expected"`
	Canonical      string `json:"canonical"`
	CollisionKey   string `json:"collisionKey"`
	CollisionGroup string `json:"collisionGroup"`
}

func TestSharedPathPolicyVector(t *testing.T) {
	encoded, err := os.ReadFile(filepath.Join("..", "testvectors", "path-policy.json"))
	if err != nil {
		t.Fatalf("load shared path-policy vector: %v", err)
	}
	var vector pathPolicyVector
	if err := json.Unmarshal(encoded, &vector); err != nil {
		t.Fatalf("decode shared path-policy vector: %v", err)
	}
	if vector.Version != 1 || vector.Kind != "path-policy" {
		t.Fatalf("unexpected vector envelope version=%d kind=%q", vector.Version, vector.Kind)
	}

	collisionGroups := make(map[string]string)
	collisionCounts := make(map[string]int)
	for _, testCase := range vector.Cases {
		t.Run(testCase.Name, func(t *testing.T) {
			if testCase.PolicyVersion != "" {
				if testCase.PolicyVersion != PathPolicyV1 || testCase.UnicodeVersion != expectedPathPolicyUnicodeVersion {
					t.Fatalf("path policy identity = %q/%q", testCase.PolicyVersion, testCase.UnicodeVersion)
				}
				return
			}

			canonical, err := CanonicalPath(testCase.Input)
			switch testCase.Expected {
			case "valid":
				if err != nil {
					t.Fatalf("CanonicalPath(%q): %v", testCase.Input, err)
				}
				if canonical != testCase.Canonical {
					t.Fatalf("canonical path = %q, want %q", canonical, testCase.Canonical)
				}
				collisionKey := siblingCollisionKey(canonical)
				if collisionKey != testCase.CollisionKey {
					t.Fatalf("collision key = %q, want %q", collisionKey, testCase.CollisionKey)
				}
				if testCase.CollisionGroup != "" {
					if previous, exists := collisionGroups[testCase.CollisionGroup]; exists && previous != collisionKey {
						t.Fatalf("collision group %q has keys %q and %q", testCase.CollisionGroup, previous, collisionKey)
					}
					collisionGroups[testCase.CollisionGroup] = collisionKey
					collisionCounts[testCase.CollisionGroup]++
				}
			case "invalid-path":
				if !errors.Is(err, ErrInvalidPath) {
					t.Fatalf("CanonicalPath(%q) error = %v", testCase.Input, err)
				}
			default:
				t.Fatalf("unsupported path-policy expectation %q", testCase.Expected)
			}
		})
	}
	for group, count := range collisionCounts {
		if count < 2 {
			t.Errorf("collision group %q contains only %d case", group, count)
		}
	}
}
