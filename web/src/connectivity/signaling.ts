export const SIGNAL_KIND_OFFER = 'offer'
export const SIGNAL_KIND_ANSWER = 'answer'
export const SIGNAL_KIND_CANDIDATE = 'candidate'

export interface ConnectivitySignal {
  readonly kind: string
  readonly payload: unknown
}

export interface SignalingRoute {
  readonly messages: ReadableStream<ConnectivitySignal>
  send(signal: ConnectivitySignal, abort?: AbortSignal): Promise<void>
}
