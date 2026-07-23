import { MAX_FRAME_BYTES } from '../../contracts/channel'
import { WebRTCDataChannelConfigurationError } from './errors'

export const DATA_CHANNEL_LABEL = 'windshare-frame-channel'
export const DATA_CHANNEL_PROTOCOL = 'windshare-v2'
export const DATA_CHANNEL_LOW_WATER_BYTES = 256 * 1024
export const DATA_CHANNEL_HIGH_WATER_BYTES = 1024 * 1024
export const TERMINAL_INTENT_CONTROL = 'terminal-intent'
export const TERMINAL_ACK_CONTROL = 'terminal-ack'

export interface DataChannelFlowControl {
  readonly lowWaterBytes: number
  readonly highWaterBytes: number
}

export const DEFAULT_DATA_CHANNEL_FLOW_CONTROL: DataChannelFlowControl = Object.freeze({
  lowWaterBytes: DATA_CHANNEL_LOW_WATER_BYTES,
  highWaterBytes: DATA_CHANNEL_HIGH_WATER_BYTES,
})

export function createWindShareDataChannel(peer: RTCPeerConnection): RTCDataChannel {
  return peer.createDataChannel(DATA_CHANNEL_LABEL, {
    ordered: true,
    protocol: DATA_CHANNEL_PROTOCOL,
    negotiated: false,
  })
}

export function validateDataChannelConfiguration(channel: RTCDataChannel): void {
  if (channel.label !== DATA_CHANNEL_LABEL) {
    throw invalidConfiguration(
      `label ${JSON.stringify(channel.label)}, want ${JSON.stringify(DATA_CHANNEL_LABEL)}`,
    )
  }
  if (channel.protocol !== DATA_CHANNEL_PROTOCOL) {
    throw invalidConfiguration(
      `protocol ${JSON.stringify(channel.protocol)}, want ${JSON.stringify(DATA_CHANNEL_PROTOCOL)}`,
    )
  }
  if (!channel.ordered) {
    throw invalidConfiguration('channel must be ordered')
  }
  if (channel.maxPacketLifeTime !== null || channel.maxRetransmits !== null) {
    throw invalidConfiguration('retransmission limits make the channel unreliable')
  }
  if (channel.negotiated) {
    throw invalidConfiguration('channel must use in-band negotiation')
  }
  if (channel.readyState !== 'connecting' && channel.readyState !== 'open') {
    throw invalidConfiguration(`initial state is ${channel.readyState}`)
  }
}

export function validateOpenedMessageCapability(peer: RTCPeerConnection): void {
  const maximum = peer.sctp?.maxMessageSize
  if (
    maximum === undefined ||
    Number.isNaN(maximum) ||
    maximum < MAX_FRAME_BYTES
  ) {
    throw invalidConfiguration(
      `SCTP maximum message size is ${String(maximum ?? 0)}, need at least ${MAX_FRAME_BYTES}`,
    )
  }
}

export function validateFlowControl(flow: DataChannelFlowControl): void {
  if (
    !Number.isSafeInteger(flow.lowWaterBytes) ||
    !Number.isSafeInteger(flow.highWaterBytes) ||
    flow.lowWaterBytes < 0 ||
    flow.highWaterBytes <= 0 ||
    flow.lowWaterBytes >= flow.highWaterBytes
  ) {
    throw invalidConfiguration(
      `flow-control profile is invalid: low=${flow.lowWaterBytes} high=${flow.highWaterBytes}`,
    )
  }
}

function invalidConfiguration(detail: string): WebRTCDataChannelConfigurationError {
  return new WebRTCDataChannelConfigurationError(
    `WebRTC DataChannel configuration is invalid: ${detail}`,
  )
}
