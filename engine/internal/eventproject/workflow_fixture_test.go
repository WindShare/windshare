package eventproject

import (
	"testing"

	"github.com/windshare/windshare/connectivity/senderrelay"
	"github.com/windshare/windshare/connectivity/v2peer"
	"github.com/windshare/windshare/core/liveshare"
	"github.com/windshare/windshare/core/session/sessionruntime"
	"github.com/windshare/windshare/core/transfer"
	"github.com/windshare/windshare/engine/internal/share"
	"github.com/windshare/windshare/engine/internal/task"
	appobs "github.com/windshare/windshare/engine/observation"
	v2 "github.com/windshare/windshare/relay/protocol/v2"
	"github.com/windshare/windshare/transport/relayv2"
	wsrtc "github.com/windshare/windshare/transport/webrtc"
)

type workflowRecorder struct {
	events        []appobs.Event
	lifecycleLoss uint64
	detailed      bool
	seen          map[workflowLossKey]uint64
}

func newWorkflowRecorder(value *workflowRecorder) *workflowRecorder { return value }
func (r *workflowRecorder) share(value share.Observation) {
	Workflow(appobs.WorkflowShare, task.Observation{TaskID: "test:1", Event: value}, r.record)
}
func (r *workflowRecorder) record(envelope task.Observation) bool {
	switch value := envelope.Event.(type) {
	case appobs.ObserverLossObserved:
		r.lifecycleLoss += value.Count()
	case appobs.LossSnapshot:
		if r.seen == nil {
			r.seen = make(map[workflowLossKey]uint64)
		}
		if value.Count > r.seen[workflowLossKey{value.Source, value.Reason}] {
			r.lifecycleLoss += value.Count - r.seen[workflowLossKey{value.Source, value.Reason}]
			r.seen[workflowLossKey{value.Source, value.Reason}] = value.Count
		}
	case appobs.Event:
		r.events = append(r.events, value)
	}
	return true
}
func (r *workflowRecorder) TraceCatalogStorage(v liveshare.CatalogStorageTrace) {
	r.share(share.Observation{Catalog: &v})
}
func (r *workflowRecorder) TraceRootPrefetch(v liveshare.RootPrefetchTrace) {
	r.share(share.Observation{Prefetch: &v})
}
func (r *workflowRecorder) TraceRelayLifecycle(v relayv2.LifecycleTrace) {
	r.share(share.Observation{RelayLifecycle: &v})
}
func (r *workflowRecorder) TraceWebRTCLifecycle(v wsrtc.LifecycleTrace) {
	r.share(share.Observation{WebRTC: &v})
}
func (r *workflowRecorder) ObserveSenderAttempt(v v2peer.SenderAttemptObservation) {
	r.share(share.Observation{SenderAttempt: &v})
}
func (r *workflowRecorder) ObserveSenderTerminalSend(v sessionruntime.SenderTerminalSendObserved) {
	r.share(share.Observation{TerminalSend: &v})
}
func (r *workflowRecorder) ObserveSenderSessionTerminated(v sessionruntime.SenderSessionTerminated) {
	r.share(share.Observation{SessionTerminal: &v})
}
func (r *workflowRecorder) ObserveRelayRecovery(_ appobs.RelayAuthority, v senderrelay.Attempt) {
	endpoint, _ := v2.NormalizeRelayEndpoint("https://relay.example")
	r.share(share.Observation{RelayRecovery: &share.RelayRecovery{Endpoint: endpoint, Attempt: v}})
}
func TestHistoricalFileSettlement(t *testing.T) {
	if kind, ok := projectFileSettlement(transfer.FilePreviouslyPublished); !ok || kind != appobs.FilePreviouslyPublished {
		t.Fatalf("historical lifecycle = (%d, %t)", kind, ok)
	}
}

type workflowLossKey struct {
	source appobs.LossSource
	reason appobs.ObserverLossReason
}
