package v2route

import (
	"bytes"
	"context"
	"crypto/sha256"
	"errors"
	"testing"
	"time"

	v2 "github.com/windshare/windshare/relay/protocol/v2"
)

func beginFixtureResume(t *testing.T, registry *Registry, fixture routeFixture) (ResumeAttempt, v2.SenderAuthority) {
	t.Helper()
	init := fixture.init
	init.Mode = v2.RegistrationResume
	attempt, err := registry.BeginResume(context.Background(), init, fixture.token)
	if err != nil {
		t.Fatal(err)
	}
	return attempt, resumeAuthority(t, fixture, init)
}

func TestResumeLiveTakeoverPreservesDescriptorAndRetiresExactParticipants(t *testing.T) {
	now := time.Unix(1_700_010_000, 0)
	registry := newRegistry(t, &now, &memoryTombstones{}, 2)
	fixture, other := makeFixture(t, 0x31), makeFixture(t, 0x41)
	oldOwner, newOwner := routeTestConnection("takeover-old"), routeTestConnection("takeover-new")
	receiver := routeTestConnection("takeover-receiver")
	otherOwner, otherReceiver := routeTestConnection("other-owner"), routeTestConnection("other-receiver")
	publishRoute(t, registry, fixture, oldOwner)
	publishRoute(t, registry, other, otherOwner)
	joined, err := registry.Join(fixture.init.ShareID, receiver)
	if err != nil {
		t.Fatal(err)
	}
	unrelated, err := registry.Join(other.init.ShareID, otherReceiver)
	if err != nil {
		t.Fatal(err)
	}

	attempt, authority := beginFixtureResume(t, registry, fixture)
	retirement, err := registry.Resume(context.Background(), attempt, authority, newOwner)
	if err != nil || retirement.Owner != oldOwner || len(retirement.Sessions) != 1 ||
		retirement.Sessions[0] != (SessionRetirement{RelaySessionID: joined.RelaySessionID, Sender: oldOwner, Receiver: receiver}) {
		t.Fatalf("takeover retirement = %+v, %v", retirement, err)
	}
	if _, changed := registry.UnexpectedDisconnect(fixture.init.ShareID, oldOwner); changed {
		t.Fatal("late old-owner cleanup changed the replacement route")
	}
	if resolved, err := registry.ResolveSession(joined.RelaySessionID, oldOwner); err != nil || resolved.Disposition != SessionRetired {
		t.Fatalf("old session = %+v, %v", resolved, err)
	}
	if resolved, err := registry.ResolveSession(unrelated.RelaySessionID, otherReceiver); err != nil ||
		resolved.Disposition != SessionForward || resolved.Destination != otherOwner {
		t.Fatalf("unrelated session changed = %+v, %v", resolved, err)
	}
	after, err := registry.Join(fixture.init.ShareID, routeTestConnection("takeover-rejoined"))
	if err != nil || after.Status != JoinReady || after.Sender != newOwner || !bytes.Equal(after.Descriptor, fixture.descriptor) {
		t.Fatalf("takeover join = %+v, %v", after, err)
	}
}

func TestResumeAttemptRejectsChangedOwnerOrRegistration(t *testing.T) {
	for _, transition := range []string{"takeover", "disconnect", "republication"} {
		t.Run(transition, func(t *testing.T) {
			now := time.Unix(1_700_011_000, 0)
			registry := newRegistry(t, &now, &memoryTombstones{}, 1)
			fixture := makeFixture(t, 0x35)
			owner := routeTestConnection("claim-owner")
			publishRoute(t, registry, fixture, owner)
			attempt, authority := beginFixtureResume(t, registry, fixture)
			switch transition {
			case "takeover":
				winner, winnerAuthority := beginFixtureResume(t, registry, fixture)
				if _, err := registry.Resume(context.Background(), winner, winnerAuthority, routeTestConnection("claim-winner")); err != nil {
					t.Fatal(err)
				}
			case "disconnect":
				registry.UnexpectedDisconnect(fixture.init.ShareID, owner)
			case "republication":
				registry.UnexpectedDisconnect(fixture.init.ShareID, owner)
				now = now.Add(SenderCrashGrace)
				// Deliberately reuse the exact owner, proving route generation is
				// independent from the connection's own generation.
				publishRoute(t, registry, fixture, owner)
			}
			if _, err := registry.Resume(context.Background(), attempt, authority, routeTestConnection("claim-late")); !errors.Is(err, ErrResumeStale) {
				t.Fatalf("late commit after %s = %v", transition, err)
			}
		})
	}
}

func TestResumeDistinguishesStartingAbsentAndInvalidCredentials(t *testing.T) {
	now := time.Unix(1_700_012_000, 0)
	registry := newRegistry(t, &now, &memoryTombstones{}, 1)
	fixture := makeFixture(t, 0x39)
	init := fixture.init
	init.Mode = v2.RegistrationResume
	owner := routeTestConnection("starting-owner")
	if err := registry.BeginRegistration(fixture.init, owner); err != nil {
		t.Fatal(err)
	}
	for _, mutation := range []string{"token", "hash", "instance", "descriptor"} {
		t.Run(mutation, func(t *testing.T) {
			changed, token := init, fixture.token
			switch mutation {
			case "token":
				token[0] ^= 1
			case "hash":
				token[0] ^= 1
				changed.ResumeTokenHash = sha256.Sum256(token[:])
			case "instance":
				changed.ShareInstance[0] ^= 1
			case "descriptor":
				changed.DescriptorDigest[0] ^= 1
			}
			if _, err := registry.BeginResume(context.Background(), changed, token); !errors.Is(err, ErrResume) {
				t.Fatalf("invalid %s = %v", mutation, err)
			}
		})
	}
	if _, err := registry.BeginResume(context.Background(), init, fixture.token); !errors.Is(err, ErrStarting) {
		t.Fatalf("unpublished route = %v", err)
	}
	now = now.Add(JoinStartingGrace)
	attempt, authority := beginFixtureResume(t, registry, fixture)
	if attempt.route != nil {
		t.Fatal("expired registration remained resumable")
	}
	if _, err := registry.Resume(context.Background(), attempt, v2.SenderAuthority{}, owner); !errors.Is(err, ErrResume) {
		t.Fatalf("unauthenticated absence = %v", err)
	}
	if _, err := registry.Resume(context.Background(), attempt, authority, owner); !errors.Is(err, ErrNotFound) {
		t.Fatalf("authenticated absence = %v", err)
	}
	publishRoute(t, registry, fixture, owner)
	if _, err := registry.Resume(context.Background(), attempt, authority, routeTestConnection("absent-late")); !errors.Is(err, ErrResumeStale) {
		t.Fatalf("absence claim replaced a later publication: %v", err)
	}
}

func TestResumeAttemptCannotCrossRegistryAuthorityOrCancellation(t *testing.T) {
	now := time.Unix(1_700_013_000, 0)
	registry := newRegistry(t, &now, &memoryTombstones{}, 1)
	other := newRegistry(t, &now, &memoryTombstones{}, 1)
	fixture := makeFixture(t, 0x3b)
	owner := routeTestConnection("authority-owner")
	publishRoute(t, registry, fixture, owner)
	attempt, authority := beginFixtureResume(t, registry, fixture)
	newOwner := routeTestConnection("authority-new")
	for _, test := range []struct {
		name      string
		registry  *Registry
		attempt   ResumeAttempt
		authority v2.SenderAuthority
		owner     ConnectionRef
	}{
		{"nil registry", nil, attempt, authority, newOwner},
		{"other registry", other, attempt, authority, newOwner},
		{"zero attempt", registry, ResumeAttempt{}, authority, newOwner},
		{"zero proof", registry, attempt, v2.SenderAuthority{}, newOwner},
		{"zero owner", registry, attempt, authority, ConnectionRef{}},
		{"same owner", registry, attempt, authority, owner},
	} {
		t.Run(test.name, func(t *testing.T) {
			if _, err := test.registry.Resume(context.Background(), test.attempt, test.authority, test.owner); !errors.Is(err, ErrResume) {
				t.Fatalf("invalid attempt = %v", err)
			}
		})
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := registry.BeginResume(ctx, attempt.init, fixture.token); !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
	if _, err := registry.Resume(ctx, attempt, authority, newOwner); !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
	if _, err := registry.Resume(context.Background(), attempt, authority, newOwner); err != nil {
		t.Fatalf("rejected attempts consumed the valid claim: %v", err)
	}
	if _, err := registry.Resume(context.Background(), attempt, authority, owner); !errors.Is(err, ErrResumeStale) {
		t.Fatalf("used attempt replay = %v", err)
	}
}

func TestResumeClaimIsFencedByStopEvenAfterDefiniteFailure(t *testing.T) {
	for _, outcome := range []CommitOutcome{CommitNotCommitted, CommitCommitted, CommitUnknown} {
		t.Run(string(rune('0'+outcome)), func(t *testing.T) {
			now := time.Unix(1_700_014_000, 0)
			store := newBlockingCommitStore()
			registry := newRegistry(t, &now, store, 1)
			fixture := makeFixture(t, 0x3d)
			publishRoute(t, registry, fixture, routeTestConnection("stop-claim-owner"))
			attempt, authority := beginFixtureResume(t, registry, fixture)
			done := startStop(registry, fixture)
			<-store.entered
			owner := routeTestConnection("stop-claim-new")
			if _, err := registry.Resume(context.Background(), attempt, authority, owner); !errors.Is(err, ErrStopping) {
				t.Fatalf("commit during STOP = %v", err)
			}
			store.replies <- commitReply{outcome: outcome}
			<-done
			want := ErrStopped
			if outcome == CommitNotCommitted {
				want = ErrResumeStale
			}
			if _, err := registry.Resume(context.Background(), attempt, authority, owner); !errors.Is(err, want) {
				t.Fatalf("commit after STOP outcome %d = %v, want %v", outcome, err, want)
			}
		})
	}
}

func TestAuthenticatedStopOnAbsentRouteFencesDelayedPublication(t *testing.T) {
	for _, outcome := range []CommitOutcome{CommitNotCommitted, CommitCommitted, CommitUnknown} {
		t.Run(string(rune('0'+outcome)), func(t *testing.T) {
			now := time.Unix(1_700_015_000, 0)
			store := newBlockingCommitStore()
			registry := newRegistry(t, &now, store, 1)
			fixture := makeFixture(t, 0x3f)
			attempt, authority := beginFixtureResume(t, registry, fixture)
			done := startStop(registry, fixture)
			<-store.entered
			owner := routeTestConnection("late-publication")
			if err := registry.BeginRegistration(fixture.init, owner); !errors.Is(err, ErrStopping) {
				t.Fatalf("delayed registration bypassed pending revocation: %v", err)
			}
			if _, err := registry.Resume(context.Background(), attempt, authority, owner); !errors.Is(err, ErrStopping) {
				t.Fatalf("absent claim bypassed pending revocation: %v", err)
			}
			other := makeFixture(t, 0x4f)
			if err := registry.BeginRegistration(other.init, owner); !errors.Is(err, ErrAdmission) {
				t.Fatalf("pending revocation escaped capacity accounting: %v", err)
			}
			store.replies <- commitReply{outcome: outcome}
			result := <-done
			if result.retirement.Owner.Valid() || len(result.retirement.Sessions) != 0 {
				t.Fatalf("absent revocation retired unrelated participants: %+v", result)
			}
			if outcome == CommitNotCommitted {
				if !errors.Is(result.err, ErrCommitFailed) {
					t.Fatal(result.err)
				}
				if err := registry.BeginRegistration(fixture.init, owner); err != nil {
					t.Fatalf("definite failure retained revocation: %v", err)
				}
			} else {
				if err := registry.BeginRegistration(fixture.init, owner); !errors.Is(err, ErrStopped) {
					t.Fatalf("revoked absence reopened: %v", err)
				}
				if _, err := registry.Resume(context.Background(), attempt, authority, owner); !errors.Is(err, ErrStopped) {
					t.Fatalf("old absence claim reopened revoked share: %v", err)
				}
			}
		})
	}
}

func TestAbsentStopHonorsCapacityAndRequiresSenderAuthority(t *testing.T) {
	now := time.Unix(1_700_016_000, 0)
	store := &memoryTombstones{}
	registry := newRegistry(t, &now, store, 1)
	fixture, occupying := makeFixture(t, 0x45), makeFixture(t, 0x55)
	if _, err := registry.Stop(context.Background(), fixture.stop, v2.StopAuthority{}); !errors.Is(err, ErrConfig) {
		t.Fatalf("unauthenticated absence revocation = %v", err)
	}
	publishRoute(t, registry, occupying, routeTestConnection("occupied"))
	if _, err := registry.Stop(context.Background(), fixture.stop, fixture.stopAuth); !errors.Is(err, ErrAdmission) {
		t.Fatalf("absent STOP bypassed route capacity = %v", err)
	}
	if store.puts != 0 {
		t.Fatal("rejected STOP reached durable storage")
	}
}
