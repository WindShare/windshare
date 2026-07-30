import { sameNetworkMatrixControlAuthority } from '../../sample-authority.ts'
import type { ExternalFixtureProbeResult } from '../../runtime-authority.ts'
import type {
  RemotePionAttemptLease,
  RemotePionAttemptResult,
} from '../remote-pion.ts'
import { REMOTE_PION_PROTOCOL_VERSION } from '../remote-pion.ts'
import {
  CONTAINED_BROWSER_SAMPLE_OUTPUT_SCHEMA,
  type ContainedBrowserPionControl,
  type ContainedBrowserSampleDependencies,
  type ContainedBrowserSampleOutput,
  type ContainedBrowserSampleSecret,
  type ContainedBrowserSession,
  type RunContainedBrowserSampleOptions,
} from './contracts.ts'
import {
  PROCESS_INSTANCE_PATTERN,
  requireBrowser,
  requireOpaqueId,
  requireProcessInstanceId,
} from './contract-validation.ts'
import { DEFAULT_CONTAINED_BROWSER_DEPENDENCIES } from './default-dependencies.ts'
import { containedBrowserCandidatePath } from './output-contract.ts'
import { parseContainedBrowserSampleSecret } from './secret-frame.ts'

export async function runContainedBrowserSample(
  options: RunContainedBrowserSampleOptions,
): Promise<ContainedBrowserSampleOutput> {
  const browser = requireBrowser(options.browser)
  const secret = parseContainedBrowserSampleSecret(options.secret)
  const sampleAuthority = secret.control.controlLease.controlAuthority.sampleAuthority
  if (sampleAuthority.browser !== browser) {
    throw new Error('contained browser engine differs from its sample authority')
  }
  const dependencies = options.dependencies ?? DEFAULT_CONTAINED_BROWSER_DEPENDENCIES
  const control = dependencies.control(secret)
  let session: ContainedBrowserSession | undefined
  let attemptId: string | undefined
  let primaryFailure: unknown
  let output: ContainedBrowserSampleOutput | undefined
  try {
    requireActive(options.signal)
    const probe = requireExactFixtureProbe(
      secret,
      await control.probeFixture(options.signal),
      dependencies.now(),
    )
    session = await dependencies.launch(browser)
    if (session.engine !== browser) {
      throw new Error('contained browser launcher returned the wrong engine')
    }
    const requestId = requireOpaqueId(dependencies.requestId())
    const lease = await control.createAttempt(
      requestId,
      secret.attemptLeaseMs,
      options.signal,
    )
    requireLeaseBinding(probe, lease, secret.control.controlLease.controlAuthority)
    const issuedAttemptId = requireOpaqueId(lease.attemptAuthority.attemptId)
    if (issuedAttemptId === requestId) {
      throw new Error('remote Pion attempt ID was not independently issued')
    }
    // Once a distinct server authority is known, every later validation branch
    // must retain it so finally can reap the exact attempt.
    attemptId = issuedAttemptId
    const frame = challengeFrame(lease)
    const offer = await session.createOffer(probe.rtcConfiguration)
    const answer = await control.offer(attemptId, offer, options.signal)
    await session.acceptAnswer(answer)
    const echo = session.exchangeChallenge(frame, secret.challengeDeadlineMs).then(
      (value) => Object.freeze({ outcome: 'received' as const, value }),
      () => Object.freeze({ outcome: 'absent' as const, value: null }),
    )
    const terminal = await pollTerminalResult(
      control,
      attemptId,
      secret,
      options.signal,
      dependencies.delay,
    )
    requireAttemptBinding(probe, terminal)
    if (terminal.terminalReceipt === null) {
      throw new Error('contained browser terminal result lacks a signed receipt')
    }
    const echoOutcome = terminal.state === 'established' ? await echo : undefined
    const challengeEchoed = echoOutcome?.outcome === 'received' && echoOutcome.value === frame
    requireExpectedTerminal(secret.expectedConnectivity, terminal, challengeEchoed)
    const browserSelectedPair = containedBrowserCandidatePath(await session.getStats())
    output = Object.freeze({
      schemaVersion: CONTAINED_BROWSER_SAMPLE_OUTPUT_SCHEMA,
      processInstanceId: requireProcessInstanceId(sampleAuthority.processInstanceId),
      browser,
      protocolResult: Object.freeze({
        runId: probe.runId,
        authorityInstanceId: probe.authorityInstanceId,
        attestationSha256: terminal.attemptAuthority.requestAuthority.fixtureBinding.attestationSha256,
        attestationPublicKeySpki: probe.attestationPublicKeySpki,
        signedAttestation: probe.signedAttestation,
        remoteServiceInstanceId:
          terminal.attemptAuthority.requestAuthority.fixtureBinding.remoteServiceInstanceId,
        networkBindingSha256:
          terminal.attemptAuthority.requestAuthority.fixtureBinding.networkBindingSha256,
        remotePeerBindingSha256:
          terminal.attemptAuthority.requestAuthority.fixtureBinding.remotePeerBindingSha256,
        controllerPublicIp: probe.controllerPublicIp,
        attestationExpiresAt: probe.leaseExpiresAt,
        remotePeerPublicIp: probe.signedAttestation.attestation.fixture.remotePeerPublicIp,
        remotePeerUdpPortMin: probe.signedAttestation.attestation.fixture.remotePeerUdpPortMin,
        remotePeerUdpPortMax: probe.signedAttestation.attestation.fixture.remotePeerUdpPortMax,
        attemptAuthority: terminal.attemptAuthority,
        state: terminal.state,
        selectedPair: terminal.selectedPair,
        challengeBindingSha256: terminal.challengeBindingSha256,
        challenge: lease.attemptAuthority.challenge,
        failureCode: terminal.failureCode,
        challengeEchoed,
        terminalReceipt: terminal.terminalReceipt,
      }),
      browserSelectedPair,
    })
  } catch (cause) {
    primaryFailure = cause
  }
  const cleanupFailures = await cleanupSample(control, attemptId, session, secret.cleanupDeadlineMs)
  if (cleanupFailures.length !== 0) {
    throw new AggregateError(
      [...(primaryFailure === undefined ? [] : [primaryFailure]), ...cleanupFailures],
      'contained browser sample cleanup did not prove terminal reaping',
    )
  }
  if (primaryFailure !== undefined) throw primaryFailure
  if (output === undefined) throw new Error('contained browser sample did not publish terminal output')
  return output
}

export function challengeFrame(input: RemotePionAttemptLease): string {
  return JSON.stringify({
    protocolVersion: REMOTE_PION_PROTOCOL_VERSION,
    attemptAuthority: input.attemptAuthority,
  })
}

async function pollTerminalResult(
  control: ContainedBrowserPionControl,
  attemptId: string,
  secret: ContainedBrowserSampleSecret,
  signal: AbortSignal,
  delay: ContainedBrowserSampleDependencies['delay'],
): Promise<RemotePionAttemptResult & { readonly state: 'established' | 'failed' }> {
  const deadline = performance.now() + secret.resultDeadlineMs
  for (;;) {
    requireActive(signal)
    const result = await control.result(attemptId, signal)
    if (result.state === 'established' || result.state === 'failed') {
      return Object.freeze({ ...result, state: result.state })
    }
    if (performance.now() >= deadline) {
      throw new Error('remote Pion attempt did not become terminal')
    }
    await delay(secret.resultPollIntervalMs, signal)
  }
}

function requireExpectedTerminal(
  expectation: ContainedBrowserSampleSecret['expectedConnectivity'],
  result: RemotePionAttemptResult & { readonly state: 'established' | 'failed' },
  challengeEchoed: boolean,
): void {
  if (
    expectation === 'established' &&
    (result.state !== 'established' || result.selectedPair === null || !challengeEchoed)
  ) throw new Error('contained browser application path was not established')
  if (
    expectation === 'blocked' &&
    (result.state !== 'failed' || result.selectedPair !== null || challengeEchoed)
  ) throw new Error('contained browser unexpectedly established connectivity')
}

async function cleanupSample(
  control: ContainedBrowserPionControl,
  attemptId: string | undefined,
  session: ContainedBrowserSession | undefined,
  deadlineMs: number,
): Promise<readonly unknown[]> {
  const failures: unknown[] = []
  if (attemptId !== undefined) {
    const controller = new AbortController()
    const timeout = setTimeout(() => controller.abort(), deadlineMs)
    try {
      await control.deleteAttempt(attemptId, controller.signal)
    } catch {
      failures.push(new Error('remote Pion attempt cleanup failed'))
    } finally {
      clearTimeout(timeout)
    }
  }
  if (session !== undefined) {
    try {
      await session.close()
    } catch {
      failures.push(new Error('contained browser process cleanup failed'))
    }
  }
  return failures
}

function requireExactFixtureProbe(
  secret: ContainedBrowserSampleSecret,
  result: ExternalFixtureProbeResult,
  now: number,
): Extract<ExternalFixtureProbeResult, { outcome: 'satisfied' }> {
  const sampleAuthority = secret.control.controlLease.controlAuthority.sampleAuthority
  if (
    result.outcome !== 'satisfied' || !PROCESS_INSTANCE_PATTERN.test(result.probeId) ||
    result.runId !== sampleAuthority.runId || result.profileId !== sampleAuthority.profileId ||
    result.authorityInstanceId !== secret.control.controlLease.authorityInstanceId ||
    result.attestationSha256 !== secret.control.controlLease.attestationSha256 ||
    Date.parse(result.leaseExpiresAt) <= now + secret.attemptLeaseMs ||
    Date.parse(secret.control.controlLease.expiresAt) <= now + secret.attemptLeaseMs ||
    Date.parse(secret.control.controlLease.expiresAt) > Date.parse(result.leaseExpiresAt)
  ) throw new Error('contained browser external fixture probe was not exact')
  return result
}

function requireLeaseBinding(
  probe: Extract<ExternalFixtureProbeResult, { outcome: 'satisfied' }>,
  lease: RemotePionAttemptLease,
  expectedControlAuthority: ContainedBrowserSampleSecret['control']['controlLease']['controlAuthority'],
): void {
  const requestAuthority = lease.attemptAuthority.requestAuthority
  if (
    !sameNetworkMatrixControlAuthority(
      requestAuthority.controlAuthority,
      expectedControlAuthority,
    ) ||
    requestAuthority.controlAuthority.sampleAuthority.runId !== probe.runId ||
    requestAuthority.fixtureBinding.attestationSha256 !== probe.attestationSha256 ||
    requestAuthority.fixtureBinding.remoteServiceInstanceId !== probe.remoteServiceInstanceId ||
    requestAuthority.fixtureBinding.networkBindingSha256 !== probe.networkBindingSha256 ||
    requestAuthority.fixtureBinding.remotePeerBindingSha256 !== probe.remotePeerBindingSha256
  ) throw new Error('remote Pion attempt lease crossed its signed fixture authority')
}

function requireAttemptBinding(
  probe: Extract<ExternalFixtureProbeResult, { outcome: 'satisfied' }>,
  result: RemotePionAttemptResult,
): void {
  const requestAuthority = result.attemptAuthority.requestAuthority
  if (
    requestAuthority.controlAuthority.sampleAuthority.runId !== probe.runId ||
    requestAuthority.fixtureBinding.attestationSha256 !== probe.attestationSha256 ||
    requestAuthority.fixtureBinding.remoteServiceInstanceId !== probe.remoteServiceInstanceId ||
    requestAuthority.fixtureBinding.networkBindingSha256 !== probe.networkBindingSha256 ||
    requestAuthority.fixtureBinding.remotePeerBindingSha256 !== probe.remotePeerBindingSha256
  ) throw new Error('remote Pion result crossed its signed fixture authority')
}

function requireActive(signal: AbortSignal): void {
  if (signal.aborted) throw new Error('contained browser sample was terminated')
}
