package accountcommand

import (
	"context"
	"fmt"
	"sync"
	"time"

	"github.com/windshare/windshare/cmd/wind/internal/humanoutput"
	"github.com/windshare/windshare/cmd/wind/internal/terminalcanvas"
	"github.com/windshare/windshare/core/serviceauthority"
	"github.com/windshare/windshare/engine/account"
)

func Describe(value account.Snapshot) string {
	switch value.State {
	case account.Verifying:
		return fmt.Sprintf("Setting up account access · %ds · %dM proof attempts", int(value.ProofElapsed.Seconds()), value.Hashes/1_000_000)
	case account.EmailRequired:
		return "Email verification required: wind account email <address>, then wind account verify <code>"
	case account.Pending:
		if !value.Active {
			return "Account setup starts when you first use official service."
		}
		return "Setting up official-service access…"
	case account.Refreshing:
		return "Refreshing account access…"
	case account.Unavailable:
		return "Account service temporarily unavailable; retrying automatically."
	case account.Denied:
		return "Official-service access denied: " + value.Reason
	case account.Ready:
		switch value.Allowance {
		case serviceauthority.AllowanceSpent:
			return "Daily relay allowance used; lower priority until " + value.ResetAt.Local().Format("Jan 2 15:04 MST") + "."
		case serviceauthority.AllowanceStale:
			return "Relay allowance is updating; transfers continue at lower priority."
		default:
			return humanoutput.FormatBytes(value.RemainingBytes) + " relay allowance remaining · resets " + value.ResetAt.Local().Format("Jan 2 15:04 MST")
		}
	default:
		return "Account access unavailable."
	}
}

// Watch owns the progress row only until initial publication, so transfer
// progress never competes with account setup for terminal presentation.
func Watch(ctx context.Context, service *account.Service, canvas *terminalcanvas.Canvas) func() {
	if service == nil {
		return func() {}
	}
	ctx, cancel := context.WithCancel(ctx)
	done := make(chan struct{})
	go func() {
		defer close(done)
		defer canvas.FinishProgress()
		ticker := time.NewTicker(time.Second)
		defer ticker.Stop()
		previous := ""
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				value, _ := service.Snapshot()
				if !value.Active || value.State == account.Ready {
					continue
				}
				message := Describe(value)
				if message != previous {
					canvas.ReplaceProgress(terminalcanvas.Plain(message))
					previous = message
				}
			}
		}
	}()
	var once sync.Once
	return func() { once.Do(func() { cancel(); <-done }) }
}
