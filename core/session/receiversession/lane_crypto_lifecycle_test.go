package receiversession

import (
	"github.com/windshare/windshare/core/session/protocolsession"
	"sync"
)

type reentrantLaneCloseChannel struct {
	protocolsession.FrameChannel
	onClose func()
	once    sync.Once
}

func (channel *reentrantLaneCloseChannel) Close() error {
	channel.once.Do(channel.onClose)
	return channel.FrameChannel.Close()
}
