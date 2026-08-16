package terminalcanvas

import "time"

// Capabilities deliberately keeps terminal properties orthogonal. In
// particular, interactivity does not imply ANSI, and ANSI does not imply color.
type Capabilities struct {
	Interactive bool
	ANSI        bool
	Color       bool
	Unicode     bool
	Columns     int
}

// CapabilityProvider is sampled at each render so resize events are observed.
type CapabilityProvider interface {
	Snapshot() Capabilities
}

// CapabilityProviderFunc is a deterministic injection seam for renderers/tests.
type CapabilityProviderFunc func() Capabilities

func (provider CapabilityProviderFunc) Snapshot() Capabilities {
	if provider == nil {
		return Capabilities{}
	}
	return provider()
}

// Clock remains separate from terminal detection because progress sampling and
// ANSI policy have unrelated authorities.
type Clock interface {
	Now() time.Time
}

// SystemClock supplies monotonic-capable time.Time values to production callers.
type SystemClock struct{}

func (SystemClock) Now() time.Time {
	return time.Now()
}

// RawTerminal exposes only the native identity used for capability detection.
// It must remain distinct from the serialized io.Writer owned by Canvas.
type RawTerminal interface {
	Fd() uintptr
}

// Environment groups independently injectable native dependencies without
// coupling the raw terminal handle to serialized output.
type Environment struct {
	RawOutput    RawTerminal
	Capabilities CapabilityProvider
	Clock        Clock
}

// NewNativeEnvironment detects capabilities from the raw output while leaving
// actual writes to a separately supplied Canvas writer.
func NewNativeEnvironment(rawOutput RawTerminal) Environment {
	return Environment{
		RawOutput:    rawOutput,
		Capabilities: newNativeCapabilities(rawOutput),
		Clock:        SystemClock{},
	}
}

type fixedCapabilities struct {
	capabilities Capabilities
}

func (provider fixedCapabilities) Snapshot() Capabilities {
	return provider.capabilities
}

func noCapabilities() CapabilityProvider {
	return fixedCapabilities{}
}
