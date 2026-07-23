// Package catalogflow provides the progressive LIST_CHILDREN service and client
// above an authenticated protocol session. It never reads a FrameChannel and
// never interprets local paths; transport and sender-object authentication are
// injected at its two narrow boundaries.
package catalogflow
