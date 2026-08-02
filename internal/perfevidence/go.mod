module github.com/windshare/windshare/internal/perfevidence

go 1.26.5

require (
	github.com/windshare/windshare v0.0.0
	golang.org/x/sys v0.47.0
	golang.org/x/text v0.40.0
)

// Performance evidence must use the process-owner implementation from the
// exact checkout being measured, independent of the root release cadence.
replace github.com/windshare/windshare => ../..
