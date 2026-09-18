package cli

import (
	"github.com/windshare/windshare/core/link"
	"io"
	"net/url"
)

const (
	browserTraceQueryParameter = "trace"
	browserTraceQueryEnabled   = "1"
)

// Browser presentation options do not grant capabilities or alter protocol identity.
type shareLinkPresentation struct {
	frontURL     string
	splitKey     bool
	browserTrace bool
}

func buildShareCapabilityPayload(capability link.Link, presentation shareLinkPresentation) ([]byte, error) {
	bare, key, err := capability.SplitURL(presentation.frontURL)
	if err != nil {
		return nil, err
	}
	page, err := url.Parse(bare)
	if err != nil {
		return nil, err
	}
	if presentation.browserTrace {
		query := page.Query()
		query.Set(browserTraceQueryParameter, browserTraceQueryEnabled)
		page.RawQuery = query.Encode()
	}
	if presentation.splitKey {
		return []byte("Bare link: " + page.String() + "\nKey: " + key + "\n"), nil
	}
	page.Fragment = key
	return []byte("Link: " + page.String() + "\n"), nil
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
