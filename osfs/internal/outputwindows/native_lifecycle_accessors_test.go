//go:build windows

package outputwindows

import (
	"errors"
	"strings"
	"testing"
)

func TestLiveStageObserverAndAmbiguousPublishErrorRetainTheirCause(t *testing.T) {
	var observed windowsLiveStageCreateCut
	observer := windowsLiveStageCreateObserverFunc(func(cut windowsLiveStageCreateCut) error {
		observed = cut
		return nil
	})
	if err := observer.ObserveLiveStageCreate(windowsLiveStageCutSynced); err != nil ||
		observed != windowsLiveStageCutSynced {
		t.Fatalf("live stage observation = (%d, %v)", observed, err)
	}

	cause := errors.New("parent durability unknown")
	failure := &windowsPublishMutationError{cause: cause}
	if !strings.Contains(failure.Error(), "may be visible") ||
		!errors.Is(failure, cause) ||
		!windowsPublicationMayBeVisible(failure) {
		t.Fatalf("ambiguous publication failure = %v", failure)
	}
}
