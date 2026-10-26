package cli

import (
	"reflect"
	"testing"

	"github.com/windshare/windshare/cmd/wind/internal/clievent"
	appobs "github.com/windshare/windshare/engine/observation"
)

func TestShareCommandFailureUsesGuaranteedPublication(t *testing.T) {
	emitter := &shareRecordingEmitter{}
	emitShareKnownFailure(emitter, ExitNetwork, appobs.FailureRelayTransport)
	if len(emitter.published) != 1 || len(emitter.events) != 0 {
		t.Fatalf("published failure = %#v, observed = %#v", emitter.published, emitter.events)
	}
	if _, ok := emitter.published[0].(clievent.CommandFailed); !ok {
		t.Fatalf("published failure type = %T", emitter.published[0])
	}
}

func TestShareReadyEventsPreserveHumanMilestoneOrder(t *testing.T) {
	emitter := &shareRecordingEmitter{}
	subject, err := appobs.NewDirectorySubject(appobs.NewDisplayName("selected-root"))
	if err != nil {
		t.Fatal(err)
	}
	selected, err := appobs.NewSharingSubjectSelected(subject)
	if err != nil {
		t.Fatal(err)
	}
	origin, err := appobs.NewRelayOrigin(appobs.RelayWSS, "relay.example", 443)
	if err != nil {
		t.Fatal(err)
	}
	connected, err := clievent.NewRelayConnected(clievent.CommandShare, origin)
	if err != nil {
		t.Fatal(err)
	}
	emitShareReady(emitter, selected, connected)
	want := []any{appobs.Ready{}, appobs.SharingSubjectSelected{}, appobs.RelayConnected{}}
	if len(emitter.published) != len(want) || len(emitter.events) != 0 {
		t.Fatalf("published ready events = %#v, observed = %#v", emitter.published, emitter.events)
	}
	for index := range want {
		if eventTypeName(emitter.published[index]) != eventTypeName(want[index]) {
			t.Fatalf("ready event %d = %T, want %T", index, emitter.published[index], want[index])
		}
	}
}

type shareRecordingEmitter struct {
	events        []clievent.Event
	published     []clievent.Event
	lifecycleLoss uint64
	detailed      bool
}

func (emitter *shareRecordingEmitter) Observe(event clievent.Event) bool {
	emitter.events = append(emitter.events, event)
	return true
}

func (emitter *shareRecordingEmitter) Publish(events ...clievent.Event) bool {
	emitter.published = append(emitter.published, events...)
	return true
}

func (emitter *shareRecordingEmitter) ReportObserverLoss(_ appobs.ObserverLossCategory, _ appobs.ObserverLossReason, count uint64) bool {
	emitter.lifecycleLoss += count
	return true
}

func (emitter *shareRecordingEmitter) ReportObservationRejection(_ appobs.ObserverLossCategory, _ appobs.ObserverLossReason, _ appobs.ObservationRejection) bool {
	emitter.lifecycleLoss++
	return true
}

func (emitter *shareRecordingEmitter) detailedDiagnosticsEnabled() bool { return emitter.detailed }

func eventTypeName(value any) string { return reflect.TypeOf(value).String() }
