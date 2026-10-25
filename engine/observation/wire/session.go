package wire

import (
	appobs "github.com/windshare/windshare/engine/observation"
)

func (enc *encoder) encodeObserverLossObserved(event appobs.ObserverLossObserved) error {
	category, err := nameOf(event.Category())
	if err != nil {
		return err
	}
	reason, err := nameOf(event.Reason())
	if err != nil {
		return err
	}
	payload := ObserverLossPayload{Category: category, Reason: reason, Count: decimal(event.Count()), OmittedSamples: decimal(event.OmittedSamples())}
	if sample, ok := event.Rejection(); ok {
		rejection := &ObservationRejectionPayload{Event: sample.Event, Source: sample.Source, Stage: sample.Stage, Field: sample.Field, Rule: sample.Rule, Session: sample.Session, Operation: sample.Operation, Revision: sample.Revision, ResponseSequence: sample.ResponseSequence, AttemptSequence: sample.AttemptSequence, Truncated: sample.Truncated(), OmittedFields: decimal(sample.OmittedFields()), OmittedBytes: decimal(sample.OmittedBytes()), Evidence: make([]RejectionField, 0)}
		for _, field := range sample.Evidence() {
			rejection.Evidence = append(rejection.Evidence, RejectionField{Field: field.Field, Representation: field.Representation, Value: field.Value})
		}
		payload.Rejection = rejection
	}
	enc.set("observer_loss", nil, payload)
	return nil
}
