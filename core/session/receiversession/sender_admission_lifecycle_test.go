package receiversession

import (
	"github.com/windshare/windshare/core/framechannel"
	"github.com/windshare/windshare/core/session/protocolsession"
	"sync"
)

type admissionBarrierChannel struct {
	protocolsession.FrameChannel
	entered chan struct{}
	once    sync.Once
}

func (channel *admissionBarrierChannel) Recv() <-chan framechannel.Frame {
	channel.once.Do(func() { close(channel.entered) })
	return channel.FrameChannel.Recv()
}
