package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"slices"
	"strings"
	"time"

	"github.com/windshare/windshare/core/link"
	"github.com/windshare/windshare/internal/networkprobe"
)

type operation struct {
	NodeKey string `json:"node_key"`
	ID      string `json:"operation_id"`
}
type drainStatus struct {
	Held      bool   `json:"held"`
	Confirmed bool   `json:"confirmed"`
	Version   uint64 `json:"policy_version"`
}
type runner struct {
	op                     operation
	authority, host, token string
	api, probe             *http.Client
}

func (r *runner) policy(ctx context.Context, action string) (drainStatus, error) {
	var status drainStatus
	err := r.call(ctx, action, r.op, &status)
	return status, err
}

func (r *runner) local(ctx context.Context, action string, args ...string) error {
	return r.remote(ctx, nil, action, args...)
}

func (r *runner) call(ctx context.Context, action string, input, out any) error {
	body, err := json.Marshal(input)
	if err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, strings.TrimRight(r.authority, "/")+"/v1/maintenance/"+action, bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+r.token)
	res, err := r.api.Do(req)
	if err != nil {
		return err
	}
	defer res.Body.Close()
	if res.StatusCode != http.StatusOK {
		return networkprobe.ReadAuthorityError(action, res)
	}
	if out == nil {
		return nil
	}
	return json.NewDecoder(io.LimitReader(res.Body, 32<<10)).Decode(out)
}

// The operator's pinned OpenSSH alias owns host access. Only validated identifiers
// and digits cross the SSH command boundary. The authority token stays on the runner.
func (r *runner) remoteCommand(ctx context.Context, input io.Reader, action string, args ...string) *exec.Cmd {
	argv := append([]string{"-o", "BatchMode=yes", "-o", "StrictHostKeyChecking=yes", "-o", "ForwardAgent=no", "-o", "ClearAllForwardings=yes", "-o", "ConnectTimeout=4", "--", r.host, "sudo", "-n", "/usr/local/sbin/windshare-install-wsrelay", action, r.op.ID, r.op.NodeKey}, args...)
	cmd := exec.CommandContext(ctx, "ssh", argv...)
	cmd.Env = slices.DeleteFunc(os.Environ(), func(entry string) bool {
		name, _, _ := strings.Cut(entry, "=")
		return strings.EqualFold(name, maintenanceTokenEnv)
	})
	cmd.Stdin = input
	cmd.Stderr = os.Stderr
	cmd.WaitDelay = time.Second
	return cmd
}

func (r *runner) remote(ctx context.Context, input io.Reader, action string, args ...string) error {
	cmd := r.remoteCommand(ctx, input, action, args...)
	cmd.Stdout = os.Stderr
	return cmd.Run()
}

func (r *runner) localRecovery(ctx context.Context) (recoveryState, error) {
	cmd := r.remoteCommand(ctx, nil, "recover")
	var out bytes.Buffer
	cmd.Stdout = &out
	if err := cmd.Run(); err != nil {
		return "", err
	}
	return parseRecoveryState(out.String())
}

func parseRecoveryState(raw string) (recoveryState, error) {
	switch state := recoveryState(strings.TrimSpace(raw)); state {
	case recoveryHeld, recoveryReleased:
		return state, nil
	default:
		return "", fmt.Errorf("unexpected host recovery state %q", state)
	}
}

func (r *runner) verify(ctx context.Context) error {
	var shares [networkprobe.OriginCount]networkprobe.SyntheticShare
	var ids [networkprobe.OriginCount][link.SenderAuthenticatedShareIDBytes]byte
	for i := range shares {
		share, err := networkprobe.NewShare()
		if err != nil {
			return err
		}
		shares[i], ids[i] = share, share.ID()
	}
	var work networkprobe.Work
	if err := r.call(ctx, "probe", map[string]any{"operation": r.op, "shares": ids}, &work); err != nil {
		return err
	}
	if work.Node.NodeKey != r.op.NodeKey || work.OperationID != r.op.ID {
		return fmt.Errorf("probe assignment changed")
	}
	return networkprobe.Check(ctx, r.probe, work, shares)
}
