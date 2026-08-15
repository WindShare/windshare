//go:build windows

package outputwindows

import (
	"errors"
	"strings"
	"testing"
)

func TestLiveStageObserverAndAmbiguousPublishErrorRetainTheirCause(t *testing.T) {
	var observed windowsV3LiveStageCreateCut
	observer := windowsV3LiveStageCreateObserverFunc(func(cut windowsV3LiveStageCreateCut) error {
		observed = cut
		return nil
	})
	if err := observer.ObserveLiveStageCreate(windowsV3LiveStageCutSynced); err != nil ||
		observed != windowsV3LiveStageCutSynced {
		t.Fatalf("live stage observation = (%d, %v)", observed, err)
	}

	cause := errors.New("parent durability unknown")
	failure := &windowsV3PublishMutationError{cause: cause}
	if !strings.Contains(failure.Error(), "may be visible") ||
		!errors.Is(failure, cause) ||
		!windowsV3PublicationMayBeVisible(failure) {
		t.Fatalf("ambiguous publication failure = %v", failure)
	}
}
