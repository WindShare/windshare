//go:build !windows

package windowsjob

import "errors"

func openSuperviseEndpoints(superviseOptions) (superviseEndpoints, error) {
	return superviseEndpoints{}, errors.New("windows process-owner endpoints are unavailable on this platform")
}
