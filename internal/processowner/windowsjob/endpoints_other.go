//go:build !windows

package windowsjob

import "errors"

func openSuperviseEndpoints(superviseOptions) (superviseEndpoints, error) {
	return superviseEndpoints{}, errors.New("Windows process-owner endpoints are unavailable on this platform")
}
