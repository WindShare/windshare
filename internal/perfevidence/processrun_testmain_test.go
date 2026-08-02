package perfevidence

import (
	"os"
	"testing"

	"github.com/windshare/windshare/internal/perfevidence/processrun"
)

func TestMain(testMain *testing.M) {
	if handled, code := processrun.MaybeRunHelper(os.Args[1:], os.Stdin); handled {
		os.Exit(code)
	}
	os.Exit(testMain.Run())
}
