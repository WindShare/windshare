package cli

import (
	"context"
	"sync"

	"github.com/windshare/windshare/cmd/wind/internal/clievent"
	"github.com/windshare/windshare/cmd/wind/internal/commandprojection"
	"github.com/windshare/windshare/cmd/wind/internal/observationbridge"
	"github.com/windshare/windshare/core/content/revisioncapacity"
	"github.com/windshare/windshare/engine"
	appobs "github.com/windshare/windshare/engine/observation"
)

type shareEventObserver interface {
	ReportObservationRejection(appobs.ObserverLossCategory, appobs.ObserverLossReason, appobs.ObservationRejection) bool
	Observe(clievent.Event) bool
	ReportObserverLoss(appobs.ObserverLossCategory, appobs.ObserverLossReason, uint64) bool
}

type shareCommandPublisher interface {
	Publish(...clievent.Event) bool
}

type shareCommandFinalizer interface {
	StageFinalization(clievent.TerminalEvent) bool
}

type detailedDiagnosticsPreference interface {
	detailedDiagnosticsEnabled() bool
}

type shareObservations struct {
	observer       shareEventObserver
	relayMu        sync.RWMutex
	relayAuthority appobs.RelayAuthority
	losses         *observationbridge.CumulativeLosses[appobs.LossSource]
}

func newShareProjection(observer shareEventObserver) *shareObservations {
	observations := &shareObservations{observer: observer}
	if preference, ok := observer.(detailedDiagnosticsPreference); ok && preference.detailedDiagnosticsEnabled() {
		observations.losses = observationbridge.NewCumulativeLosses[appobs.LossSource](observer)
	}
	return observations
}
func (observations *shareObservations) SetRelayAuthority(authority appobs.RelayAuthority) {
	if observations == nil || !authority.Valid() {
		return
	}
	observations.relayMu.Lock()
	observations.relayAuthority = authority
	observations.relayMu.Unlock()
}

func (observations *shareObservations) RelayAuthority() appobs.RelayAuthority {
	if observations == nil {
		return appobs.RelayAuthority{}
	}
	observations.relayMu.RLock()
	defer observations.relayMu.RUnlock()
	return observations.relayAuthority
}

func (observations *shareObservations) TraceCapacity(value revisioncapacity.TraceEvent) {
	event, err := engine.DescribeCapacity(value)
	observations.emitProjected(appobs.ObserverLossSenderCapacity, event, err)
}

func (observations *shareObservations) capacityTracer() revisioncapacity.Tracer {
	if !observations.detailedDiagnosticsEnabled() {
		return nil
	}
	return observations
}

func (observations *shareObservations) emitProjected(category appobs.ObserverLossCategory, event appobs.Event, err error) {
	observations.emitProjectedContext(context.Background(), nil, category, event, err)
}

func (observations *shareObservations) emitProjectedContext(
	ctx context.Context,
	gate *observationbridge.PublicationGate,
	category appobs.ObserverLossCategory,
	event appobs.Event,
	err error,
) bool {
	if observations == nil || observations.observer == nil {
		return false
	}
	return gate.Commit(ctx, func() bool {
		if err != nil || event == nil {
			observations.projectionFailed(category, err)
			return false
		}
		wrapped, err := clievent.FromObservation(event)
		if err != nil {
			observations.projectionFailed(category, err)
			return false
		}
		return observations.observer.Observe(wrapped)
	})
}

func (observations *shareObservations) projectionFailed(category appobs.ObserverLossCategory, cause error) {
	if observations == nil || observations.observer == nil {
		return
	}
	observations.observer.ReportObservationRejection(category, commandprojection.ObserverLossReason(cause), commandprojection.ProjectionRejection(cause))
}

func (observations *shareObservations) detailedDiagnosticsEnabled() bool {
	if observations == nil || observations.observer == nil {
		return false
	}
	preference, ok := observations.observer.(detailedDiagnosticsPreference)
	return ok && preference.detailedDiagnosticsEnabled()
}

func (observations *shareObservations) reportCumulativeLoss(
	source appobs.LossSource,
	category appobs.ObserverLossCategory,
	reason appobs.ObserverLossReason,
	cumulative uint64,
) {
	if observations == nil || observations.observer == nil {
		return
	}
	if observations.losses != nil {
		observations.losses.Report(source, category, reason, cumulative)
		return
	}
	observations.observer.ReportObserverLoss(category, reason, cumulative)
}

func (observations *shareObservations) reportReaderStatus(
	category appobs.ObserverLossCategory,
	status observationbridge.Status,
) {
	if observations == nil || observations.observer == nil || status.Joined {
		return
	}
	residue := status.Buffered
	if status.Active {
		residue = saturatingAdd(residue, 1)
	}
	if residue == 0 {
		residue = 1
	}
	observations.observer.ReportObserverLoss(category, appobs.ObserverLossReaderNotJoined, residue)
}

func mustShareFailure(code appobs.FailureCode) appobs.Failure {
	failure, err := appobs.NewFailure(code)
	if err != nil {
		panic("cli: invalid internal share failure code")
	}
	return failure
}
