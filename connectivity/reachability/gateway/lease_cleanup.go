package gateway

import (
	"context"
	"errors"

	r "github.com/windshare/windshare/connectivity/reachability"
)

// Invalid installed resources lose publication authority even if best-effort
// deletion fails. Cleanup must outlive the demand that authorized creation.
func revokeInvalid(ctx context.Context, gateway r.Gateway, request r.Request, lease r.Lease) error {
	cleanup, cancel := context.WithTimeout(context.WithoutCancel(ctx), r.DefaultOperationTimeout)
	defer cancel()
	return errors.Join(r.ErrLeaseLost, r.ErrInvalidResponse, gateway.Delete(cleanup, request, lease))
}
