package share

import (
	"slices"

	"github.com/windshare/windshare/connectivity/relayset"
	"github.com/windshare/windshare/core/catalog"
)

// SourceOmissionSummary aggregates skipped source items across committed
// directories discovered so far for authoritative snapshot projection.
type SourceOmissionSummary struct {
	Count   uint64
	Samples []catalog.SourceOmission
}

// Startup never waits and retains failure as well as successful first readiness.
func (c *Controller) Startup() (Ready, error, bool) {
	select {
	case <-c.readyDone:
		return cloneReady(c.ready), c.readyErr, true
	default:
		return Ready{}, nil, false
	}
}

func (c *Controller) bindRelayStatus(read func() relayset.SenderStatus) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.finished {
		return
	}
	c.relayStatus, c.hasRelayStatus = read, true
}

// RelayStatus reads the relay owner independently of observation delivery.
func (c *Controller) RelayStatus() (relayset.SenderStatus, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.relayStatus != nil {
		return c.relayStatus(), true
	}
	return c.finalRelayStatus, c.hasRelayStatus
}

// RecordSourceOmissions records one committed directory's omissions at most
// once per non-zero DirectoryID, keeping samples bounded for UI summaries.
func (c *Controller) RecordSourceOmissions(summary catalog.SourceOmissions) bool {
	if summary.Count == 0 {
		return false
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.finished {
		return false
	}
	if !summary.Directory.IsZero() {
		if c.omittedDirs == nil {
			c.omittedDirs = make(map[catalog.DirectoryID]struct{})
		}
		if _, recorded := c.omittedDirs[summary.Directory]; recorded {
			return false
		}
		c.omittedDirs[summary.Directory] = struct{}{}
	}
	if summary.Count > ^uint64(0)-c.omittedCount {
		c.omittedCount = ^uint64(0)
	} else {
		c.omittedCount += summary.Count
	}
	if remaining := catalog.MaxSourceOmissionSamples - len(c.omittedSamples); remaining > 0 && len(summary.Samples) > 0 {
		c.omittedSamples = append(c.omittedSamples, summary.Samples[:min(len(summary.Samples), remaining)]...)
	}
	return true
}

// SourceOmissions returns the authoritative omission summary when any item has
// been skipped across discovered directories.
func (c *Controller) SourceOmissions() (SourceOmissionSummary, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.omittedCount == 0 {
		return SourceOmissionSummary{}, false
	}
	return SourceOmissionSummary{
		Count:   c.omittedCount,
		Samples: slices.Clone(c.omittedSamples),
	}, true
}
