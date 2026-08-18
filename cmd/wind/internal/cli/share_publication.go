package cli

import (
	"errors"
	"io"

	"github.com/windshare/windshare/core/link"
)

var errSharePublicationPlan = errors.New("share capability publication plan is incomplete")

type sharePublicationStage uint8

const (
	sharePublicationBuildFailed sharePublicationStage = iota + 1
	sharePublicationWriteFailed
	sharePublicationPrivateReadyFailed
)

type sharePublicationFailure struct {
	stage sharePublicationStage
	cause error
}

func (failure *sharePublicationFailure) Error() string {
	return "share capability publication failed"
}

func (failure *sharePublicationFailure) Unwrap() error { return failure.cause }

type sharePublicationPlan struct {
	buildPayload        func() ([]byte, error)
	publishPayload      func([]byte) error
	stopRuntime         func()
	startRootPrefetch   func()
	publishPrivateReady func() error
	publishPublicReady  func()
}

// executeSharePublication is the single readiness cut. Keeping the complete
// payload write ahead of every warm-up and readiness callback prevents a short
// stdout write from leaving an operational sender that the caller cannot use.
func executeSharePublication(plan sharePublicationPlan) error {
	if plan.buildPayload == nil || plan.publishPayload == nil || plan.stopRuntime == nil || plan.startRootPrefetch == nil ||
		plan.publishPrivateReady == nil || plan.publishPublicReady == nil {
		return &sharePublicationFailure{stage: sharePublicationBuildFailed, cause: errSharePublicationPlan}
	}
	payload, err := plan.buildPayload()
	if err != nil {
		plan.stopRuntime()
		return &sharePublicationFailure{stage: sharePublicationBuildFailed, cause: err}
	}
	defer clear(payload)
	if err := plan.publishPayload(payload); err != nil {
		plan.stopRuntime()
		return &sharePublicationFailure{stage: sharePublicationWriteFailed, cause: err}
	}
	plan.startRootPrefetch()
	if err := plan.publishPrivateReady(); err != nil {
		plan.stopRuntime()
		return &sharePublicationFailure{stage: sharePublicationPrivateReadyFailed, cause: err}
	}
	plan.publishPublicReady()
	return nil
}

func buildShareCapabilityPayload(capability link.Link, frontURL string, split bool) ([]byte, error) {
	if split {
		bare, key, err := capability.SplitURL(frontURL)
		if err != nil {
			return nil, err
		}
		return []byte("Bare link: " + bare + "\nKey: " + key + "\n"), nil
	}
	full, err := capability.URL(frontURL)
	if err != nil {
		return nil, err
	}
	return []byte("Link: " + full + "\n"), nil
}

func publishShareCapability(writer io.Writer, payload []byte) error {
	if writer == nil || len(payload) == 0 {
		return io.ErrShortWrite
	}
	written, err := writer.Write(payload)
	if err != nil {
		return err
	}
	if written != len(payload) {
		return io.ErrShortWrite
	}
	return nil
}
