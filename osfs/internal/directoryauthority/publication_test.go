package directoryauthority

import (
	"context"
	"errors"
	"testing"

	"github.com/windshare/windshare/osfs/internal/fileexecution"
)

func TestPublicationCapabilityExpiresBeforeReturningToCaller(t *testing.T) {
	for _, fail := range []bool{false, true} {
		t.Run(map[bool]string{false: "published", true: "callback-failed"}[fail], func(t *testing.T) {
			fixture := newFileAuthorityFixture(t, fileAuthorityFixtureOptions{nested: true, preexisting: true})
			fixture.objects.publishMutates = true
			guards := fixture.platform.guardCalls
			handles := fixture.platform.openDirectories.Load()
			injected := errors.New("publication interrupted")
			var borrowed fileexecution.FilePublication
			ctx := context.Background()
			err := fixture.destination.WithPublication(ctx, &fileAuthorityOwnedFile{object: fixture.object},
				fixture.expectation, func(publication fileexecution.FilePublication) error {
					borrowed = publication
					if fail {
						return injected
					}
					if final, err := publication.Observe(ctx); err != nil || final.Condition() != fileexecution.FinalAbsent {
						t.Fatalf("before publication = %v, %v", final.Condition(), err)
					}
					if final, err := publication.PublishNoReplace(ctx); err != nil || final.Condition() != fileexecution.FinalOwnedAtExpectedSize {
						t.Fatalf("publication = %v, %v", final.Condition(), err)
					}
					return publication.Sync(ctx)
				})
			if fail && !errors.Is(err, injected) || !fail && err != nil {
				t.Fatalf("publication callback = %v", err)
			}
			if fixture.platform.guardCalls != guards+1 || fixture.platform.openDirectories.Load() != handles {
				t.Fatalf("publication retained ancestry or acquired redundant guards: guards %d -> %d, handles %d -> %d",
					guards, fixture.platform.guardCalls, handles, fixture.platform.openDirectories.Load())
			}
			if _, err := borrowed.Observe(ctx); !errors.Is(err, ErrAuthorityClosed) {
				t.Fatalf("escaped observation capability = %v", err)
			}
			if _, err := borrowed.PublishNoReplace(ctx); !errors.Is(err, ErrAuthorityClosed) {
				t.Fatalf("escaped publication capability = %v", err)
			}
			if err := borrowed.Sync(ctx); !errors.Is(err, ErrAuthorityClosed) {
				t.Fatalf("escaped durability capability = %v", err)
			}
		})
	}
}
