package testprocess

import "github.com/windshare/windshare/internal/testtrace"

// EventSink is retained at the fixture facade so existing child fixtures do not
// need to depend on process ownership. Its implementation and semantics live in
// the independent testtrace module.
type EventSink = testtrace.EventSink

var OpenEventSink = testtrace.OpenEventSink
