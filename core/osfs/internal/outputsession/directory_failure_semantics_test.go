package outputsession

import (
	"context"
	"errors"
	"testing"

	"github.com/windshare/windshare/core/transfer"
	"github.com/windshare/windshare/core/transfer/ordinaryoutput"
)

func TestInvariantFailureRequiresOperationAttention(t *testing.T) {
	session := &Session{}
	err := session.markInvariantFailureLocked()
	if !errors.Is(err, ErrExecutorContract) || !session.attention ||
		!session.requiredFault.Valid() {
		t.Fatalf("invariant failure = (attention %t, fault %v, %v)",
			session.attention, session.requiredFault, err)
	}
}

func TestDirectoryAdmissionRejectsUnboundAndCanceledRequestsBeforeMutation(t *testing.T) {
	t.Run("nil-context", func(t *testing.T) {
		fixture := newTestFixture(t, nil)
		if _, err := fixture.session.AdmitDirectory(nil, fixture.directoryRequest(
			fixture.rootDirectory,
		)); !errors.Is(err, ErrInvalidConfiguration) {
			t.Fatalf("nil context = %v", err)
		}
		if calls, _ := fixture.directories.counts(); calls != 0 {
			t.Fatalf("nil context mutated destination %d times", calls)
		}
	})
	t.Run("canceled-context", func(t *testing.T) {
		fixture := newTestFixture(t, nil)
		ctx, cancel := context.WithCancel(context.Background())
		cancel()
		if _, err := fixture.session.AdmitDirectory(
			ctx, fixture.directoryRequest(fixture.rootDirectory),
		); !errors.Is(err, context.Canceled) {
			t.Fatalf("canceled context = %v", err)
		}
		if calls, _ := fixture.directories.counts(); calls != 0 {
			t.Fatalf("canceled context mutated destination %d times", calls)
		}
	})
	t.Run("rejected-projection", func(t *testing.T) {
		fixture := newTestFixture(t, nil)
		if _, err := fixture.session.AdmitDirectory(
			context.Background(), transfer.DirectoryMaterializationRequest{},
		); !errors.Is(err, ErrDirectoryBinding) {
			t.Fatalf("rejected projection = %v", err)
		}
	})
}

func TestDirectoryBindingRejectsEveryCanonicalizationSubstitution(t *testing.T) {
	t.Run("invalid-artifact", func(t *testing.T) {
		fixture := newTestFixture(t, nil)
		if _, _, _, err := fixture.session.bindMaterializedArtifact(
			ordinaryoutput.ArtifactPath{},
		); !errors.Is(err, ErrDirectoryBinding) {
			t.Fatalf("invalid artifact = %v", err)
		}
	})
	t.Run("artifact-locator-error", func(t *testing.T) {
		failure := errors.New("artifact locator rejected")
		fixture := newTestFixture(t, func(config *Config) {
			config.Locator = &fakeDirectoryAuthority{canonicalLocatorKey: func(string) (string, error) {
				return "", failure
			}}
		})
		if _, err := fixture.session.AdmitDirectory(
			context.Background(), fixture.directoryRequest(fixture.rootDirectory),
		); !errors.Is(err, failure) {
			t.Fatalf("artifact locator error = %v", err)
		}
	})
	t.Run("artifact-locator-substitution", func(t *testing.T) {
		fixture := newTestFixture(t, func(config *Config) {
			config.Locator = &fakeDirectoryAuthority{canonicalLocatorKey: func(string) (string, error) {
				return string([]byte{0xff}), nil
			}}
		})
		if _, err := fixture.session.AdmitDirectory(
			context.Background(), fixture.directoryRequest(fixture.rootDirectory),
		); !errors.Is(err, ErrExecutorContract) {
			t.Fatalf("artifact locator substitution = %v", err)
		}
	})
	t.Run("destination-error", func(t *testing.T) {
		failure := errors.New("destination rejected")
		fixture := newTestFixture(t, func(config *Config) {
			config.Destinations = ArtifactDestinationBinderFunc(func(
				ordinaryoutput.ArtifactPath,
			) (DestinationPath, error) {
				return DestinationPath{}, failure
			})
		})
		if _, err := fixture.session.AdmitDirectory(
			context.Background(), fixture.directoryRequest(fixture.rootDirectory),
		); !errors.Is(err, failure) {
			t.Fatalf("destination error = %v", err)
		}
	})
	t.Run("destination-invalid", func(t *testing.T) {
		fixture := newTestFixture(t, func(config *Config) {
			config.Destinations = ArtifactDestinationBinderFunc(func(
				ordinaryoutput.ArtifactPath,
			) (DestinationPath, error) {
				return DestinationPath{}, nil
			})
		})
		if _, err := fixture.session.AdmitDirectory(
			context.Background(), fixture.directoryRequest(fixture.rootDirectory),
		); !errors.Is(err, ErrExecutorContract) {
			t.Fatalf("invalid destination = %v", err)
		}
	})
	t.Run("destination-locator-substitution", func(t *testing.T) {
		fixture := newTestFixture(t, func(config *Config) {
			config.Destinations = ArtifactDestinationBinderFunc(func(
				ordinaryoutput.ArtifactPath,
			) (DestinationPath, error) {
				return NewDestinationPath("renamed")
			})
			config.Locator = &fakeDirectoryAuthority{canonicalLocatorKey: func(path string) (string, error) {
				if path == "renamed" {
					return "", nil
				}
				return "locator:" + path, nil
			}}
		})
		if _, err := fixture.session.AdmitDirectory(
			context.Background(), fixture.directoryRequest(fixture.rootDirectory),
		); !errors.Is(err, ErrExecutorContract) {
			t.Fatalf("destination locator substitution = %v", err)
		}
	})
	t.Run("canceled-after-binding", func(t *testing.T) {
		ctx, cancel := context.WithCancel(context.Background())
		fixture := newTestFixture(t, func(config *Config) {
			config.Destinations = ArtifactDestinationBinderFunc(func(
				ordinaryoutput.ArtifactPath,
			) (DestinationPath, error) {
				cancel()
				return NewDestinationSessionRoot(), nil
			})
		})
		if _, err := fixture.session.AdmitDirectory(
			ctx, fixture.directoryRequest(fixture.rootDirectory),
		); !errors.Is(err, context.Canceled) {
			t.Fatalf("post-binding cancellation = %v", err)
		}
	})
}

func TestDirectoryExecutorAmbiguityQuarantinesOnlyTheClaim(t *testing.T) {
	failure := errors.New("directory materialization failed")
	for name, materialize := range map[string]func(
		context.Context, DirectoryClaim,
	) (DirectoryMaterialization, error){
		"missing-cut": func(context.Context, DirectoryClaim) (DirectoryMaterialization, error) {
			return DirectoryMaterialization{}, failure
		},
		"stable-error": func(context.Context, DirectoryClaim) (DirectoryMaterialization, error) {
			return DirectoryMaterialization{Cut: MutationStable}, failure
		},
		"invalid-success": func(context.Context, DirectoryClaim) (DirectoryMaterialization, error) {
			return DirectoryMaterialization{Cut: MutationNoChange}, nil
		},
	} {
		t.Run(name, func(t *testing.T) {
			fixture := newTestFixture(t, nil)
			fixture.directories.materialize = materialize
			if _, err := fixture.session.AdmitDirectory(
				context.Background(), fixture.directoryRequest(fixture.rootDirectory),
			); err == nil {
				t.Fatal("ambiguous directory result was accepted")
			}
			if calls, _ := fixture.directories.counts(); calls != 1 {
				t.Fatalf("directory executor calls = %d", calls)
			}
		})
	}
	if _, _, err := directoryProjection(ordinaryoutput.ArtifactPathProjection{}); !errors.Is(
		err, ErrDirectoryBinding,
	) {
		t.Fatalf("zero projection = %v", err)
	}
	if path, materialize, err := directoryProjection(
		ordinaryoutput.TraverseOnlyProjection(),
	); err != nil || materialize || path.Valid() {
		t.Fatalf("traverse-only projection = (%q, %t, %v)", path.String(), materialize, err)
	}
}
