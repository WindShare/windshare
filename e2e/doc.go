// Package e2e owns black-box CLI process validation. TestCritical provides the
// single small sender/relay/receiver path used by ordinary CI; TestLong retains
// the multi-process catalog, lane-cut, recovery, and lifecycle scenarios for the
// weekly gate. Short mode runs only the package's cheap harness contracts.
package e2e
