package sendersession

import (
	"crypto/ed25519"

	"github.com/windshare/windshare/core/observationstream"
	sessioncontract "github.com/windshare/windshare/core/session"
	"github.com/windshare/windshare/core/session/internal/sessioncore"
	sessionobs "github.com/windshare/windshare/core/session/observation"
	"github.com/windshare/windshare/core/session/protocolsession"
)

type responseSigner struct{ privateKey ed25519.PrivateKey }

func (signer responseSigner) PrepareSenderControl(
	binding protocolsession.ControlBinding,
	kind protocolsession.MessageKind,
	id *protocolsession.OperationID,
	body []byte,
) (protocolsession.PreparedSenderControl, error) {
	return protocolsession.PrepareSenderControl(signer.privateKey, binding, kind, id, body)
}

func newResponses(
	runtime *sessioncore.Runtime,
	key ed25519.PrivateKey,
	terminalObserver sessionobs.SenderTerminalSendObserver,
	output observationstream.Producer[sessionobs.ProtocolObservation],
) (*sessioncore.Responses, error) {
	if runtime == nil {
		return nil, sessioncontract.ErrRuntimeConfig
	}
	observations := newResponseObservations(output)
	config := sessioncore.ResponseConfig{ControlPreparer: responseSigner{privateKey: key}}
	if observations != nil {
		// The core joins response handlers and physical writers before finalizers.
		// Draining here preserves late settlements without a receiver-side worker.
		if err := runtime.AddFinalizer(observations.close); err != nil {
			observations.close()
			return nil, err
		}
		config.Settlements = observations.producer
	}
	if terminalObserver != nil {
		config.TerminalObserver = func(event sessioncore.TerminalSendObservation) {
			observeSenderTerminalSend(terminalObserver, event)
		}
	}
	return sessioncore.NewResponses(runtime, config), nil
}

const receiptObservationCapacity observationstream.Capacity = 64

type responseObservations struct {
	producer observationstream.Producer[protocolsession.SendAttemptSettlement]
	output   observationstream.Producer[sessionobs.ProtocolObservation]
	done     chan struct{}
}

func newResponseObservations(output observationstream.Producer[sessionobs.ProtocolObservation]) *responseObservations {
	if output.IsZero() {
		return nil
	}
	producer, consumer, _ := observationstream.New[protocolsession.SendAttemptSettlement](receiptObservationCapacity)
	observations := &responseObservations{producer: producer, output: output, done: make(chan struct{})}
	go func() {
		defer close(observations.done)
		for settled := range consumer {
			correlation := settled.Correlation()
			ctx := sessionobs.ProtocolObservationContext{Correlation: sessionobs.ProtocolObservationCorrelation{
				Role: protocolsession.RoleSender, ProtocolSessionID: correlation.ProtocolSessionID,
				OperationID: correlation.OperationID, RequestKind: correlation.RequestKind}, ObservedAt: settled.ObservedAt()}
			output.TryPublish(sessionobs.NewSendAttemptSettled(ctx, correlation.ResponseKind, settled.Attempt()))
		}
	}()
	return observations
}

func (observations *responseObservations) close() {
	completion := observations.producer.Complete()
	<-observations.done
	observations.output.RecordDropped(completion.CapacityDropped)
}
