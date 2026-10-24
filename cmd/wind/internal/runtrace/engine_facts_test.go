package runtrace

import (
	"github.com/windshare/windshare/adapters/filesystem"
	"github.com/windshare/windshare/cmd/wind/internal/clievent"
	"github.com/windshare/windshare/core/session/sessionruntime"
	"github.com/windshare/windshare/engine"
	appobs "github.com/windshare/windshare/engine/observation"
	"github.com/windshare/windshare/osfs"
)

func traceProtocolObservation(command clievent.Command, v sessionruntime.ProtocolObservation) (clievent.ProtocolObservationObserved, error) {
	event, err := engine.DescribeProtocolObservation(appobs.Workflow(command), v)
	return clievent.ProtocolObservationObserved{ProtocolObservationObserved: event}, err
}
func traceFilesystemOutput(v osfs.FilesystemOutputTrace) (clievent.FilesystemOutputObserved, error) {
	event, err := filesystem.ProjectFilesystemOutput(v)
	return clievent.FilesystemOutputObserved{FilesystemOutputObserved: event}, err
}
func traceSenderSessionTerminated(v sessionruntime.SenderSessionTerminated) (clievent.SenderSessionTerminated, error) {
	event, err := engine.DescribeSenderTermination(v)
	return clievent.SenderSessionTerminated{SenderSessionTerminated: event}, err
}
