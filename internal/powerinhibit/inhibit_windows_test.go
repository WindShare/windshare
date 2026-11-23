//go:build windows

package powerinhibit

import "testing"

func TestWindowsPowerRequestLifecycle(t *testing.T) {
	t.Parallel()

	inhibitor := New()
	release, err := inhibitor.InhibitSleep("WindShare unit test sleep inhibition")
	if err != nil {
		t.Fatalf("InhibitSleep failed: %v", err)
	}
	// Release twice to confirm idempotency against kernel32 PowerClearRequest/CloseHandle.
	release()
	release()
}
