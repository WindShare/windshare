package engine

import (
	"errors"
	"fmt"

	"github.com/windshare/windshare/core/link"
)

// ReceiveLinkInput keeps the separate key explicit so it cannot silently
// override a different key embedded in the URL.
type ReceiveLinkInput struct {
	URL         string
	SeparateKey string
}

var (
	ErrInvalidReceiveLink      = errors.New("invalid receive link")
	ErrReceiveKeyRequired      = errors.New("receive link requires a key")
	ErrReceiveKeyConflict      = errors.New("receive link keys conflict")
	ErrUnsupportedReceiveSuite = errors.New("unsupported receive link suite")
)

// ResolveReceiveLink performs no I/O: clients can ask for a missing key before
// creating a task or acquiring output authority. Protocol causes remain
// available through errors.Is for diagnostics.
func ResolveReceiveLink(input ReceiveLinkInput) (link.Link, error) {
	var capability link.Link
	var err error
	if input.SeparateKey != "" {
		capability, err = link.Merge(input.URL, input.SeparateKey)
	} else {
		capability, err = link.Parse(input.URL)
	}
	if err == nil {
		return capability, nil
	}

	kind := ErrInvalidReceiveLink
	switch {
	case errors.Is(err, link.ErrMissingFragment):
		kind = ErrReceiveKeyRequired
	case errors.Is(err, link.ErrKeyConflict):
		kind = ErrReceiveKeyConflict
	case errors.Is(err, link.ErrUnknownSuite):
		kind = ErrUnsupportedReceiveSuite
	}
	return link.Link{}, fmt.Errorf("%w: %w", kind, err)
}
