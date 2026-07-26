package channeltest

import (
	"context"
	"strings"
	"testing"

	"github.com/windshare/windshare/core/framechannel"
)

type fixtureContractChannel struct{}

func (fixtureContractChannel) Send(context.Context, framechannel.Frame) error { return nil }
func (fixtureContractChannel) SendTerminal(context.Context, framechannel.Frame) error {
	return nil
}
func (fixtureContractChannel) Recv() <-chan framechannel.Frame {
	frames := make(chan framechannel.Frame)
	close(frames)
	return frames
}
func (fixtureContractChannel) State() framechannel.ChannelState { return framechannel.Open }
func (fixtureContractChannel) Close() error                     { return nil }

func TestFixtureContractReportsTheMissingAuthority(t *testing.T) {
	valid := Fixture{
		Channel: fixtureContractChannel{},
		ReceiveSent: func(context.Context) (SentFrame, error) {
			return SentFrame{}, nil
		},
		Deliver:         func(framechannel.Frame) error { return nil },
		DeliverTerminal: func(framechannel.Frame) error { return nil },
		RemoteClose:     func() error { return nil },
		SaturateSends:   func(testing.TB) {},
		ReleaseSends:    func() {},
		Cleanup:         func() {},
	}
	if err := validateFixture(valid); err != nil {
		t.Fatal(err)
	}

	for _, testCase := range []struct {
		name   string
		remove func(*Fixture)
	}{
		{name: "Channel", remove: func(fixture *Fixture) { fixture.Channel = nil }},
		{name: "ReceiveSent", remove: func(fixture *Fixture) { fixture.ReceiveSent = nil }},
		{name: "Deliver", remove: func(fixture *Fixture) { fixture.Deliver = nil }},
		{name: "DeliverTerminal", remove: func(fixture *Fixture) { fixture.DeliverTerminal = nil }},
		{name: "RemoteClose", remove: func(fixture *Fixture) { fixture.RemoteClose = nil }},
		{name: "SaturateSends", remove: func(fixture *Fixture) { fixture.SaturateSends = nil }},
		{name: "ReleaseSends", remove: func(fixture *Fixture) { fixture.ReleaseSends = nil }},
		{name: "Cleanup", remove: func(fixture *Fixture) { fixture.Cleanup = nil }},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			invalid := valid
			testCase.remove(&invalid)
			err := validateFixture(invalid)
			if err == nil || !strings.Contains(err.Error(), testCase.name) {
				t.Fatalf("fixture error=%v, want missing %s", err, testCase.name)
			}
		})
	}
}

func TestPatternedFrameOwnsEmptyAndNonEmptyShapes(t *testing.T) {
	if frame := patternedFrame(0x31, 0); frame == nil || len(frame) != 0 {
		t.Fatalf("empty frame=%v", frame)
	}
	frame := patternedFrame(0x31, 3)
	if len(frame) != 3 || frame[0] != 0x31 || frame[1] == frame[2] {
		t.Fatalf("patterned frame=%x", frame)
	}
	mutate(frame)
	if frame[0] != 0xce {
		t.Fatalf("mutated marker=%x", frame[0])
	}
}
