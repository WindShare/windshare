const operationTimeoutMs = 30_000

function withTimeout(promise, label) {
  let timer
  return Promise.race([
    promise,
    new Promise((_, reject) => {
      timer = setTimeout(() => reject(new Error(`${label} timed out`)), operationTimeoutMs)
    }),
  ]).finally(() => clearTimeout(timer))
}

function once(target, event, label) {
  return withTimeout(new Promise((resolve, reject) => {
    target.addEventListener(event, resolve, { once: true })
    target.addEventListener('error', (value) => reject(value.error ?? new Error(`${label} failed`)), { once: true })
  }), label)
}

function patternedFrame(marker, size) {
  const frame = new Uint8Array(size)
  if (size === 0) return frame
  frame[0] = marker
  for (let index = 1; index < size; index += 1) frame[index] = (index * 31 + 17) % 251
  return frame
}

function validPattern(frame, marker, size) {
  if (frame.length !== size || size === 0 || frame[0] !== marker) return false
  for (let index = 1; index < size; index += 1) {
    if (frame[index] !== (index * 31 + 17) % 251) return false
  }
  return true
}

async function fetchJSON(path, options) {
  const response = await fetch(path, options)
  const text = await response.text()
  if (!response.ok) throw new Error(`${options?.method ?? 'GET'} ${path} failed: ${text}`)
  return text === '' ? undefined : JSON.parse(text)
}

window.runD1Interop = async () => {
  const config = await fetchJSON('/config')
  const malformedSetting = config.scenario === 'malformed-setting'
  const peer = new RTCPeerConnection({ iceServers: [] })
  const channel = peer.createDataChannel(config.channelLabel, {
    ordered: true,
    protocol: malformedSetting ? config.invalidProtocol : config.channelProtocol,
  })
  channel.binaryType = 'arraybuffer'
  channel.bufferedAmountLowThreshold = config.lowWaterBytes

  const events = []
  const errors = []
  let expectTerminal = false
  let serverProbeReceived = false
  let serverBurstMessages = 0
  let canceledSendReceived = false
  let cancellationBarrierReceived = false
  let remoteCloseSendReceived = false
  let browserCloseInitiated = false
  let browserOpened = false
  let lowObserved = false
  let clientBurstMessages = 0
  let clientBufferPeak = channel.bufferedAmount
  let terminalResolve
  let terminalReject
  const terminalReceived = new Promise((resolve, reject) => {
    terminalResolve = resolve
    terminalReject = reject
  })
  const closed = once(channel, 'close', 'DataChannel close').then(() => {
    events.push('channel-closed')
  })
  channel.addEventListener('open', () => {
    browserOpened = true
  }, { once: true })

  channel.addEventListener('message', (event) => {
    try {
      if (typeof event.data === 'string') {
        if (event.data !== config.terminalIntent || expectTerminal) {
          throw new Error(`unexpected text control ${event.data}`)
        }
        expectTerminal = true
        events.push('terminal-intent')
        return
      }
      const frame = new Uint8Array(event.data)
      if (expectTerminal) {
        if (!validPattern(frame, config.serverTerminalMarker, config.terminalFrameBytes)) {
          throw new Error('terminal frame changed')
        }
        events.push('terminal-frame')
        channel.send(config.terminalAck)
        events.push('terminal-ack-sent')
        terminalResolve()
        return
      }
      if (validPattern(frame, config.serverProbeMarker, config.maxFrameSize)) {
        serverProbeReceived = true
        events.push('server-probe')
        return
      }
      if (frame.length === config.maxFrameSize && frame[0] === config.serverBurstMarker) {
        serverBurstMessages += 1
        return
      }
      if (frame.length === 1 && frame[0] === config.canceledSendMarker) {
        canceledSendReceived = true
        events.push('canceled-send-received')
        return
      }
      if (frame.length === 1 && frame[0] === config.cancellationBarrier) {
        cancellationBarrierReceived = true
        events.push('cancellation-barrier-received')
        return
      }
      if (frame.length === 1 && frame[0] === config.remoteCloseMarker) {
        remoteCloseSendReceived = true
        events.push('remote-close-send-received')
        return
      }
      if (frame.length === 1 && frame[0] === config.serverFinishedMarker) {
        events.push('server-burst-finished')
        return
      }
      throw new Error(`unexpected binary frame len=${frame.length} marker=${frame[0] ?? 0}`)
    } catch (error) {
      errors.push(error.message)
      terminalReject(error)
      throw error
    }
  })
  channel.addEventListener('error', (event) => {
    errors.push(event.error?.message ?? 'unknown DataChannel error')
  })

  const opened = malformedSetting ? undefined : once(channel, 'open', 'DataChannel open')
  const gatheringComplete = peer.iceGatheringState === 'complete'
    ? Promise.resolve()
    : once(peer, 'icegatheringstatechange', 'ICE gathering').then(async function waitComplete() {
      if (peer.iceGatheringState === 'complete') return
      await once(peer, 'icegatheringstatechange', 'ICE gathering')
      return waitComplete()
    })
  const offer = await peer.createOffer()
  await peer.setLocalDescription(offer)
  await gatheringComplete
  const answer = await fetchJSON('/offer', {
    method: 'POST',
    headers: { 'Content-Type': 'application/json' },
    body: JSON.stringify(peer.localDescription),
  })
  await peer.setRemoteDescription(answer)

  if (malformedSetting) {
    await closed
    const server = await fetchJSON('/result')
    const sctpMaxMessageSize = peer.sctp?.maxMessageSize ?? 0
    const channelState = channel.readyState
    peer.close()
    const peerClosed = peer.connectionState === 'closed'
    events.push('browser-peer-closed')
    return {
      browser: {
        label: channel.label,
        protocol: channel.protocol,
        ordered: channel.ordered,
        reliable: channel.maxPacketLifeTime === null && channel.maxRetransmits === null,
        negotiated: channel.negotiated,
        sctpMaxMessageSize,
        browserOpened,
        channelState,
        peerClosed,
        events,
        errors,
      },
      server,
    }
  }

  await opened
  events.push('channel-open')

  // The request is pending before outbound saturation begins, minimizing the
  // distance between the adapter's capacity-wait event and browser close.
  const closeAction = config.scenario === 'remote-close' ? fetchJSON('/action') : undefined

  channel.send(patternedFrame(config.clientProbeMarker, config.maxFrameSize))
  const low = once(channel, 'bufferedamountlow', 'browser bufferedamountlow').then(() => {
    lowObserved = true
  })
  const burst = patternedFrame(config.clientBurstMarker, config.maxFrameSize)
  clientBufferPeak = channel.bufferedAmount
  while (clientBufferPeak < config.highWaterBytes && clientBurstMessages < config.maximumBursts) {
    channel.send(burst)
    clientBurstMessages += 1
    clientBufferPeak = Math.max(clientBufferPeak, channel.bufferedAmount)
  }
  if (clientBufferPeak < config.highWaterBytes) throw new Error(`browser buffer peaked at ${clientBufferPeak}`)
  await low
  channel.send(Uint8Array.of(config.clientFinishedMarker))

  if (config.scenario === 'remote-close') {
    const instruction = await closeAction
    if (instruction.action !== 'close-data-channel') {
      throw new Error(`unexpected server action ${instruction.action}`)
    }
    events.push('browser-close-initiated')
    browserCloseInitiated = true
    channel.close()
    await closed
  } else {
    await withTimeout(terminalReceived, 'production terminal')
    await closed
  }
  const server = await fetchJSON('/result')
  const sctpMaxMessageSize = peer.sctp?.maxMessageSize ?? 0
  const channelState = channel.readyState
  peer.close()
  const peerClosed = peer.connectionState === 'closed'
  return {
    browser: {
      label: channel.label,
      protocol: channel.protocol,
      ordered: channel.ordered,
      reliable: channel.maxPacketLifeTime === null && channel.maxRetransmits === null,
      negotiated: channel.negotiated,
      sctpMaxMessageSize,
      browserOpened,
      channelState,
      peerClosed,
      clientBurstMessages,
      clientBufferPeak,
      lowObserved,
      serverProbeReceived,
      serverBurstMessages,
      canceledSendReceived,
      cancellationBarrierReceived,
      remoteCloseSendReceived,
      browserCloseInitiated,
      events,
      errors,
    },
    server,
  }
}
