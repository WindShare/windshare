// Package e2e owns black-box CLI process validation. The user-trace critical
// sender/relay/receiver scenario is the single ordinary-CI path; TestLong retains
// the multi-process catalog, lane-cut, recovery, and lifecycle scenarios for the
// weekly gate. Short mode runs only the package's cheap harness contracts.
package e2e
