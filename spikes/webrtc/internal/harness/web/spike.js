const operationTimeoutMs = 20_000
const pollIntervalMs = 20

function delay(ms) {
  return new Promise((resolve) => setTimeout(resolve, ms))
}

async function withTimeout(promise, label, timeoutMs = operationTimeoutMs) {
  let timer
  try {
    return await Promise.race([
      promise,
      new Promise((_, reject) => {
        timer = setTimeout(() => reject(new Error(`${label} timed out after ${timeoutMs} ms`)), timeoutMs)
      }),
    ])
  } finally {
    clearTimeout(timer)
  }
}

async function waitFor(predicate, label, timeoutMs = operationTimeoutMs) {
  const deadline = performance.now() + timeoutMs
  while (performance.now() < deadline) {
    const value = await predicate()
    if (value) return value
    await delay(pollIntervalMs)
  }
  throw new Error(`${label} timed out after ${timeoutMs} ms`)
}

async function fetchJSON(path, options = undefined) {
  const response = await fetch(path, options)
  const text = await response.text()
  if (!response.ok) {
    throw new Error(`${options?.method ?? 'GET'} ${path} failed (${response.status}): ${text}`)
  }
  return text === '' ? undefined : JSON.parse(text)
}

function signalEnvelope(config, kind, payload) {
  return {
    type: 'signal',
    sessionId: config.sessionId,
    kind,
    payload,
  }
}

function patternedFrame(marker, size) {
  const frame = new Uint8Array(size)
  if (size === 0) return frame
  frame[0] = marker
  for (let i = 1; i < frame.length; i += 1) {
    frame[i] = (i * 31 + 17) % 251
  }
  return frame
}

function validPattern(frame, marker, size) {
  if (frame.length !== size || size === 0 || frame[0] !== marker) return false
  for (let i = 1; i < frame.length; i += 1) {
    if (frame[i] !== (i * 31 + 17) % 251) return false
  }
  return true
}

async function postSignal(signal) {
  return fetchJSON('/signal', {
    method: 'POST',
    headers: { 'Content-Type': 'application/json' },
    body: JSON.stringify(signal),
  })
}

async function applyPionCandidates(peer, config) {
  let cursor = 0
  let applied = 0
  for (;;) {
    const batch = await fetchJSON(`/signals?after=${cursor}`)
    for (const signal of batch.signals) {
      if (signal.type !== 'signal' || signal.sessionId !== config.sessionId || signal.kind !== 'candidate') {
        throw new Error(`invalid Pion candidate envelope: ${JSON.stringify(signal)}`)
      }
      await peer.addIceCandidate(signal.payload)
      applied += 1
    }
    cursor = batch.next
    if (batch.complete) return applied
    await delay(pollIntervalMs)
  }
}

async function waitForServerResult() {
  return waitFor(async () => {
    const result = await fetchJSON('/result')
    return result.channelClosed ? result : undefined
  }, 'Pion close observation')
}

window.runWindShareSpike = async () => {
  const config = await fetchJSON('/config')
  const peer = new RTCPeerConnection({ iceServers: [] })
  const localCandidates = []
  const controls = []
  const events = []
  const errors = []
  let gatheringComplete
  const gatheringDone = new Promise((resolve) => {
    gatheringComplete = resolve
  })
  peer.addEventListener('icecandidate', (event) => {
    if (event.candidate) {
      localCandidates.push(event.candidate.toJSON())
    } else {
      gatheringComplete()
    }
  })
  peer.addEventListener('connectionstatechange', () => {
    events.push(`peer-connection-${peer.connectionState}`)
    if (peer.connectionState === 'failed') {
      errors.push('Chromium peer connection entered failed state')
    }
  })

  const channel = peer.createDataChannel(config.channelLabel, {
    ordered: true,
    protocol: config.channelProtocol,
  })
  channel.binaryType = 'arraybuffer'
  channel.bufferedAmountLowThreshold = config.lowWaterMark

  let serverProbeValid = false
  let serverBurstMessages = 0
  let serverTerminalReceived = false
  let channelClosed = false
  channel.addEventListener('message', (event) => {
    if (typeof event.data === 'string') {
      controls.push(JSON.parse(event.data))
      return
    }
    const frame = new Uint8Array(event.data)
    if (validPattern(frame, config.serverProbeMarker, config.maxFrameSize)) {
      serverProbeValid = true
      return
    }
    if (frame.length === config.maxFrameSize && frame[0] === config.serverBurstMarker) {
      serverBurstMessages += 1
      return
    }
    if (validPattern(frame, config.serverTerminalMarker, config.terminalFrameBytes)) {
      serverTerminalReceived = true
      events.push('server-terminal-received')
      channel.send(JSON.stringify({ type: 'server-terminal-received' }))
      return
    }
    throw new Error(`unexpected Pion binary message len=${frame.length} marker=${frame[0] ?? 0}`)
  })
  channel.addEventListener('close', () => {
    channelClosed = true
    events.push('channel-closed')
  })
  channel.addEventListener('error', (event) => {
    const detail = event.error?.message ?? 'unknown RTC error'
    errors.push(`Chromium DataChannel error: ${detail}`)
    events.push('channel-error')
  })

  const offer = await peer.createOffer()
  await peer.setLocalDescription(offer)
  const answerSignal = await postSignal(signalEnvelope(config, 'offer', offer))
  if (answerSignal.type !== 'signal' || answerSignal.sessionId !== config.sessionId || answerSignal.kind !== 'answer') {
    throw new Error(`invalid Pion answer envelope: ${JSON.stringify(answerSignal)}`)
  }
  await peer.setRemoteDescription(answerSignal.payload)

  await withTimeout(gatheringDone, 'Chromium ICE gathering')
  let echoedBrowserCandidates = 0
  for (const candidate of localCandidates) {
    const sent = signalEnvelope(config, 'candidate', candidate)
    const echoed = await postSignal(sent)
    if (echoed.type !== sent.type || echoed.sessionId !== sent.sessionId || echoed.kind !== sent.kind || echoed.payload.candidate !== candidate.candidate) {
      throw new Error(`candidate did not round-trip through WindShare signal schema: ${JSON.stringify(echoed)}`)
    }
    echoedBrowserCandidates += 1
  }
  const appliedPionCandidates = await applyPionCandidates(peer, config)

  await waitFor(() => channel.readyState === 'open', 'DataChannel open')
  await waitFor(() => controls.some((control) => control.type === 'server-ready'), 'server-ready control')

  channel.send(patternedFrame(config.clientProbeMarker, config.maxFrameSize))
  await waitFor(() => controls.some((control) => control.type === 'client-probe-accepted'), '64 KiB browser probe acknowledgement')

  let browserLowEvent = false
  const browserLow = new Promise((resolve) => {
    channel.addEventListener('bufferedamountlow', () => {
      browserLowEvent = true
      resolve()
    }, { once: true })
  })
  const browserBurst = patternedFrame(config.clientBurstMarker, config.maxFrameSize)
  let browserBurstMessages = 0
  let browserPeak = channel.bufferedAmount
  while (browserPeak < config.highWaterMark && browserBurstMessages < config.maxBurstMessages) {
    channel.send(browserBurst)
    browserBurstMessages += 1
    browserPeak = Math.max(browserPeak, channel.bufferedAmount)
  }
  if (browserPeak < config.highWaterMark) {
    throw new Error(`Chromium bufferedAmount only reached ${browserPeak}`)
  }
  await withTimeout(browserLow, 'Chromium bufferedamountlow')
  await waitFor(() => channel.bufferedAmount <= config.lowWaterMark, 'Chromium buffer drain')
  channel.send(JSON.stringify({
    type: 'client-burst-complete',
    count: browserBurstMessages,
    peak: browserPeak,
    lowEvent: browserLowEvent,
  }))
  await waitFor(() => controls.some((control) => control.type === 'client-burst-accepted'), 'browser burst acknowledgement')

  channel.send(JSON.stringify({ type: 'start-server-burst' }))
  const serverBurst = await waitFor(() => controls.find((control) => control.type === 'server-burst-complete' || control.type === 'server-burst-failed'), 'Pion burst result')
  if (serverBurst.type === 'server-burst-failed') {
    throw new Error(serverBurst.message)
  }
  if (!serverProbeValid || serverBurstMessages !== serverBurst.count) {
    throw new Error(`Pion burst delivery mismatch: probe=${serverProbeValid} received=${serverBurstMessages} expected=${serverBurst.count}`)
  }

  channel.send(patternedFrame(config.clientTerminalMarker, config.terminalFrameBytes))
  await waitFor(() => serverTerminalReceived, 'server terminal frame')
  await waitFor(() => channelClosed, 'DataChannel close after terminal')
  const server = await waitForServerResult()
  const sctpMaxMessageSize = peer.sctp?.maxMessageSize ?? 0
  peer.close()

  return {
    browser: {
      ordered: channel.ordered,
      reliable: channel.maxRetransmits === null && channel.maxPacketLifeTime === null,
      negotiated: channel.negotiated,
      label: channel.label,
      protocol: channel.protocol,
      chromeCandidates: echoedBrowserCandidates,
      pionCandidates: appliedPionCandidates,
      sctpMaxMessageSize,
      browserBurstMessages,
      browserBackpressurePeak: browserPeak,
      browserBufferedAmountLow: browserLowEvent,
      serverProbeValid,
      serverBurstMessages,
      serverBackpressurePeak: serverBurst.peak,
      serverBufferedAmountLow: serverBurst.lowEvent,
      events,
      errors,
    },
    server,
  }
}
