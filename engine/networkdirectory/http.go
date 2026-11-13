package networkdirectory

import (
	"context"
	"fmt"
	"io"
	"net/http"

	"github.com/windshare/windshare/core/nodedirectory"
)

func fetch(ctx context.Context, address string) ([]byte, error) {
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, address, nil)
	if err != nil {
		return nil, err
	}
	client := &http.Client{Timeout: MirrorTimeout, CheckRedirect: func(request *http.Request, via []*http.Request) error {
		if len(via) > 3 || request.URL.Scheme != "https" {
			return fmt.Errorf("invalid directory mirror redirect")
		}
		return nil
	}}
	response, err := client.Do(request)
	if err != nil {
		return nil, err
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("directory mirror HTTP %d", response.StatusCode)
	}
	data, err := io.ReadAll(io.LimitReader(response.Body, nodedirectory.MaximumEnvelopeBytes+1))
	if err != nil {
		return nil, err
	}
	if len(data) > nodedirectory.MaximumEnvelopeBytes {
		return nil, nodedirectory.ErrFormat
	}
	return data, nil
}
