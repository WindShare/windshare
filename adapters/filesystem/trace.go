package filesystem

import (
	"github.com/windshare/windshare/core/diagnostics"
	"github.com/windshare/windshare/osfs"
)

func traceOutput(sink diagnostics.Sink) osfs.FilesystemOutputTracer {
	if !sink.Enabled() {
		return nil
	}
	return osfs.FilesystemOutputTraceFunc(func(value osfs.FilesystemOutputTrace) { sink.Record(value) })
}
