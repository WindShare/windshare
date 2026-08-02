import { request as httpsRequest } from 'node:https'
import { isProxy } from 'node:util/types'

import type { NetworkMatrixProfileId } from '../vocabulary.ts'

const MAXIMUM_OIDC_RESPONSE_BYTES = 1_048_576
const CANONICAL_ID_PATTERN = /^[a-z][a-z0-9]*(?:-[a-z0-9]+)*$/u
const OPAQUE_ID_PATTERN = /^[A-Za-z0-9_-]{16,128}$/u
const REPOSITORY_PATTERN = /^[A-Za-z0-9_.-]+\/[A-Za-z0-9_.-]+$/u
const WORKFLOW_REF_PATTERN = /^[A-Za-z0-9_.-]+\/[A-Za-z0-9_.-]+\/\.github\/workflows\/[A-Za-z0-9_./-]+@refs\/(?:heads|tags)\/[A-Za-z0-9_./-]+$/u
const IDENTITY_CONSTRUCTOR_AUTHORITY = Symbol('GitHubActionsOidcIdentityAuthority')

export const PARENT_WORKLOAD_IDENTITY_PROTOCOL =
  'windshare.browser-network-matrix.parent-workload-identity/v1' as const

export interface ParentWorkloadIdentityBinding {
  readonly protocolVersion: typeof PARENT_WORKLOAD_IDENTITY_PROTOCOL
  readonly kind: 'github-actions-oidc'
  readonly audience: string
  readonly issuer: string
  readonly repository: string
  readonly ref: string
  readonly workflowRef: string
  readonly requestOrigin: string
  readonly requestPath: string
  readonly requestQuery: string
}

export interface ParentWorkloadIdentitySettlementReceipt {
  readonly terminal: 'closed'
}

export interface ParentWorkloadIdentityAuthority {
  readonly binding: ParentWorkloadIdentityBinding
  issue(input: {
    readonly runId: string
    readonly profileId: NetworkMatrixProfileId
    readonly probeNonce: string
    readonly signal: AbortSignal
  }): Promise<Uint8Array>
  closeAndWait(): Promise<ParentWorkloadIdentitySettlementReceipt>
  forceTerminateAndWait(): Promise<ParentWorkloadIdentitySettlementReceipt>
}

export interface GitHubActionsOidcIdentityOptions {
  readonly audience: string
  readonly issuer: string
  readonly repository: string
  readonly ref: string
  readonly workflowRef: string
  readonly requestOrigin: string
  readonly requestPath: string
  readonly requestQuery: string
}

export interface GitHubActionsOidcIdentityTestHarnessOptions
extends GitHubActionsOidcIdentityOptions {
  readonly environment: NodeJS.ProcessEnv
  readonly request: typeof requestGithubActionsOidcToken
}

interface CapturedGitHubActionsOidcRequestBootstrap {
  readonly kind: 'request-bootstrap'
  readonly requestUrl: string
  readonly requestToken: Uint8Array
  readonly request: typeof requestGithubActionsOidcToken
}

export const MINTED_GITHUB_ACTIONS_OIDC_PROTOCOL =
  'windshare.browser-network-matrix.minted-oidc/v1' as const

export interface MintedGitHubActionsOidcEnvelope {
  readonly protocolVersion: typeof MINTED_GITHUB_ACTIONS_OIDC_PROTOCOL
  readonly audience: string
  readonly requestOrigin: string
  readonly requestPath: string
  readonly requestQuery: string
  readonly assertion: Uint8Array
}

interface CapturedMintedGitHubActionsOidcBootstrap extends MintedGitHubActionsOidcEnvelope {
  readonly kind: 'minted-bootstrap'
}

type CapturedGitHubActionsOidcBootstrap =
  CapturedGitHubActionsOidcRequestBootstrap | CapturedMintedGitHubActionsOidcBootstrap

/**
 * Captures and deletes both ambient runner sentinels as one erasable lease. The
 * lease remains the outer owner after consumption, so a failure before or during
 * runtime bootstrap can still terminate the issued identity authority exactly.
 */
export class GitHubActionsOidcBootstrapLease {
  #captured: CapturedGitHubActionsOidcBootstrap | undefined
  #authority: GitHubActionsOidcIdentityAuthority | undefined
  #terminalReceipt: ParentWorkloadIdentitySettlementReceipt | undefined
  #closeOperation: Promise<ParentWorkloadIdentitySettlementReceipt> | undefined
  #forceOperation: Promise<ParentWorkloadIdentitySettlementReceipt> | undefined

  static fromMintedEnvelope(envelope: MintedGitHubActionsOidcEnvelope): GitHubActionsOidcBootstrapLease {
    return new GitHubActionsOidcBootstrapLease(requireMintedEnvelope(envelope))
  }

  static captureTestHarness(
    environment: NodeJS.ProcessEnv,
    request: typeof requestGithubActionsOidcToken,
  ): GitHubActionsOidcBootstrapLease {
    return new GitHubActionsOidcBootstrapLease(Object.freeze({
      kind: 'request-bootstrap',
      ...takeOidcBootstrapEnvironment(environment),
      request,
    }))
  }

  private constructor(captured: CapturedGitHubActionsOidcBootstrap) {
    this.#captured = captured
  }

  consume(options: GitHubActionsOidcIdentityOptions): GitHubActionsOidcIdentityAuthority {
    if (this.#terminalReceipt !== undefined || this.#authority !== undefined) {
      throw new Error('GitHub Actions OIDC bootstrap lease is no longer consumable')
    }
    const captured = this.#captured
    if (captured === undefined) throw new Error('GitHub Actions OIDC bootstrap lease is unavailable')
    this.#captured = undefined
    try {
      const authority = new GitHubActionsOidcIdentityAuthority(
        IDENTITY_CONSTRUCTOR_AUTHORITY,
        options,
        captured,
      )
      this.#authority = authority
      return authority
    } catch (cause) {
      eraseCapturedBootstrap(captured)
      throw cause
    }
  }

  closeAndWait(): Promise<ParentWorkloadIdentitySettlementReceipt> {
    if (this.#terminalReceipt !== undefined) return Promise.resolve(this.#terminalReceipt)
    if (this.#forceOperation !== undefined) return this.#forceOperation
    if (this.#closeOperation !== undefined) return this.#closeOperation
    const operation = this.#settle(false).catch((cause: unknown) => {
      if (this.#closeOperation === operation) this.#closeOperation = undefined
      throw cause
    })
    this.#closeOperation = operation
    return operation
  }

  forceTerminateAndWait(): Promise<ParentWorkloadIdentitySettlementReceipt> {
    if (this.#terminalReceipt !== undefined) return Promise.resolve(this.#terminalReceipt)
    if (this.#forceOperation !== undefined) return this.#forceOperation
    const gracefulToJoin = this.#closeOperation
    const operation = this.#settle(true).then(async (receipt) => {
      await gracefulToJoin?.catch(() => undefined)
      return receipt
    }).catch((cause: unknown) => {
      if (this.#forceOperation === operation) this.#forceOperation = undefined
      throw cause
    })
    this.#forceOperation = operation
    return operation
  }

  async #settle(force: boolean): Promise<ParentWorkloadIdentitySettlementReceipt> {
    const captured = this.#captured
    this.#captured = undefined
    if (captured !== undefined) eraseCapturedBootstrap(captured)
    const authority = this.#authority
    if (authority !== undefined) {
      const receipt = force
        ? await authority.forceTerminateAndWait()
        : await authority.closeAndWait()
      if (!exactIdentityClosedReceipt(receipt)) {
        throw new Error('GitHub Actions OIDC identity authority did not settle its bootstrap lease')
      }
    }
    this.#terminalReceipt ??= Object.freeze({ terminal: 'closed' })
    return this.#terminalReceipt
  }
}

/**
 * GitHub's bootstrap token is captured once and removed from process.env before
 * any broker or Playwright child exists. Only a short-lived minted assertion and
 * its exact endpoint/audience binding cross the broker's anonymous descriptor.
 */
export class GitHubActionsOidcIdentityAuthority implements ParentWorkloadIdentityAuthority {
  readonly binding: ParentWorkloadIdentityBinding
  readonly #audience: string
  readonly #requestUrl: string
  readonly #requestToken: Uint8Array
  readonly #request: typeof requestGithubActionsOidcToken
  readonly #mintedAssertion: Uint8Array | undefined
  readonly #activeIssues = new Set<{
    readonly controller: AbortController
    readonly settled: Promise<void>
  }>()
  #accepting = true
  #terminalReceipt: ParentWorkloadIdentitySettlementReceipt | undefined
  #closeOperation: Promise<ParentWorkloadIdentitySettlementReceipt> | undefined
  #forceOperation: Promise<ParentWorkloadIdentitySettlementReceipt> | undefined

  static createTestHarness(
    options: GitHubActionsOidcIdentityTestHarnessOptions,
  ): GitHubActionsOidcIdentityAuthority {
    return GitHubActionsOidcBootstrapLease
      .captureTestHarness(options.environment, options.request)
      .consume(options)
  }

  constructor(
    authority: typeof IDENTITY_CONSTRUCTOR_AUTHORITY,
    options: GitHubActionsOidcIdentityOptions,
    captured: CapturedGitHubActionsOidcBootstrap,
  ) {
    if (authority !== IDENTITY_CONSTRUCTOR_AUTHORITY) {
      throw new Error('GitHub Actions OIDC identity construction authority is invalid')
    }
    let requestToken = Buffer.alloc(0)
    try {
      const audience = requireAudience(options.audience)
      const issuer = requireCanonicalHttpsOrigin(options.issuer, 'issuer')
      const requestOrigin = requireCanonicalHttpsOrigin(options.requestOrigin, 'request origin')
      const requestPath = requireAbsoluteRequestPath(options.requestPath)
      const requestQuery = requireCanonicalRequestQuery(options.requestQuery)
      if (
        typeof options.repository !== 'string' || !REPOSITORY_PATTERN.test(options.repository) ||
        typeof options.ref !== 'string' || !/^refs\/(?:heads|tags)\/[A-Za-z0-9_./-]+$/u.test(options.ref) ||
        typeof options.workflowRef !== 'string' || !WORKFLOW_REF_PATTERN.test(options.workflowRef)
      ) throw new Error('GitHub Actions OIDC claim authority is invalid')
      let requestUrl = ''
      let request: typeof requestGithubActionsOidcToken = requestGithubActionsOidcToken
      let mintedAssertion: Uint8Array | undefined
      if (captured.kind === 'request-bootstrap') {
        const endpoint = new URL(captured.requestUrl)
        if (
          endpoint.protocol !== 'https:' || endpoint.username !== '' || endpoint.password !== '' ||
          endpoint.hash !== '' || endpoint.origin !== requestOrigin || endpoint.pathname !== requestPath ||
          endpoint.search !== requestQuery
        ) throw new Error('GitHub Actions OIDC endpoint is invalid')
        requestUrl = endpoint.toString()
        requestToken = captured.requestToken
        request = captured.request
      } else {
        if (
          captured.audience !== audience ||
          captured.requestOrigin !== requestOrigin || captured.requestPath !== requestPath ||
          captured.requestQuery !== requestQuery
        ) throw new Error('minted GitHub Actions OIDC endpoint binding is invalid')
        mintedAssertion = captured.assertion
      }
      this.binding = Object.freeze({
        protocolVersion: PARENT_WORKLOAD_IDENTITY_PROTOCOL,
        kind: 'github-actions-oidc',
        audience,
        issuer,
        repository: options.repository,
        ref: options.ref,
        workflowRef: options.workflowRef,
        requestOrigin,
        requestPath,
        requestQuery,
      })
      this.#audience = audience
      this.#requestUrl = requestUrl
      this.#requestToken = requestToken
      this.#request = request
      this.#mintedAssertion = mintedAssertion
    } catch (cause) {
      eraseCapturedBootstrap(captured)
      throw cause
    }
  }

  async issue(input: {
    readonly runId: string
    readonly profileId: NetworkMatrixProfileId
    readonly probeNonce: string
    readonly signal: AbortSignal
  }): Promise<Uint8Array> {
    requireScope(input.runId, input.profileId, input.probeNonce)
    if (!this.#accepting || input.signal.aborted) {
      throw new Error('parent workload identity request was terminated')
    }
    if (this.#mintedAssertion !== undefined) {
      return Uint8Array.from(this.#mintedAssertion)
    }
    const controller = new AbortController()
    const abort = (): void => controller.abort()
    input.signal.addEventListener('abort', abort, { once: true })
    if (input.signal.aborted) controller.abort()
    let markSettled: (() => void) | undefined
    const settled = new Promise<void>((resolve) => { markSettled = resolve })
    const active = Object.freeze({ controller, settled })
    this.#activeIssues.add(active)
    try {
      const assertion = await this.#request({
        endpoint: oidcEndpoint(this.#requestUrl, this.#audience),
        requestToken: this.#requestToken,
        signal: controller.signal,
      })
      if (controller.signal.aborted || !this.#accepting) {
        assertion.fill(0)
        throw new Error('parent workload identity request was terminated')
      }
      if (!isOidcAssertion(assertion)) {
        assertion.fill(0)
        throw new Error('GitHub Actions OIDC assertion is invalid')
      }
      return assertion
    } finally {
      input.signal.removeEventListener('abort', abort)
      this.#activeIssues.delete(active)
      markSettled?.()
    }
  }

  closeAndWait(): Promise<ParentWorkloadIdentitySettlementReceipt> {
    this.#accepting = false
    this.#closeOperation ??= this.#settle()
    return this.#closeOperation
  }

  forceTerminateAndWait(): Promise<ParentWorkloadIdentitySettlementReceipt> {
    this.#accepting = false
    this.#forceOperation ??= this.#settle()
    return this.#forceOperation
  }

  async #settle(): Promise<ParentWorkloadIdentitySettlementReceipt> {
    if (this.#terminalReceipt !== undefined) return this.#terminalReceipt
    const active = [...this.#activeIssues]
    for (const issue of active) issue.controller.abort()
    try {
      await Promise.all(active.map(({ settled }) => settled))
    } finally {
      this.#requestToken.fill(0)
      this.#mintedAssertion?.fill(0)
    }
    this.#terminalReceipt ??= Object.freeze({ terminal: 'closed' })
    return this.#terminalReceipt
  }
}

function exactIdentityClosedReceipt(
  value: unknown,
): value is ParentWorkloadIdentitySettlementReceipt {
  if (typeof value !== 'object' || value === null || Array.isArray(value)) return false
  const receipt = value as Record<string, unknown>
  const keys = Object.keys(receipt)
  return keys.length === 1 && keys[0] === 'terminal' && receipt.terminal === 'closed'
}

function requireMintedEnvelope(value: unknown): CapturedMintedGitHubActionsOidcBootstrap {
  if (
    typeof value !== 'object' || value === null || isProxy(value) || Array.isArray(value) ||
    Object.getPrototypeOf(value) !== Object.prototype
  ) throw new Error('minted GitHub Actions OIDC envelope is invalid')
  const descriptors = Object.getOwnPropertyDescriptors(value)
  const names = Reflect.ownKeys(descriptors)
  const expected = [
    'assertion', 'audience', 'protocolVersion', 'requestOrigin', 'requestPath', 'requestQuery',
  ]
  if (
    names.some((name) => typeof name !== 'string') || names.length !== expected.length ||
    !expected.every((name) => names.includes(name))
  ) throw new Error('minted GitHub Actions OIDC envelope is invalid')
  for (const name of expected) {
    const descriptor = descriptors[name]
    if (descriptor === undefined || !Object.hasOwn(descriptor, 'value') || descriptor.enumerable !== true) {
      throw new Error('minted GitHub Actions OIDC envelope is active')
    }
  }
  if (descriptors.protocolVersion?.value !== MINTED_GITHUB_ACTIONS_OIDC_PROTOCOL) {
    throw new Error('minted GitHub Actions OIDC protocol is invalid')
  }
  const assertionValue: unknown = descriptors.assertion?.value
  if (isProxy(assertionValue) || !(assertionValue instanceof Uint8Array)) {
    throw new Error('minted GitHub Actions OIDC assertion is invalid')
  }
  const assertion = Uint8Array.from(assertionValue)
  if (!isOidcAssertion(assertion)) {
    assertion.fill(0)
    throw new Error('minted GitHub Actions OIDC assertion is invalid')
  }
  try {
    return Object.freeze({
      kind: 'minted-bootstrap',
      protocolVersion: MINTED_GITHUB_ACTIONS_OIDC_PROTOCOL,
      audience: requireAudience(descriptors.audience?.value),
      requestOrigin: requireCanonicalHttpsOrigin(descriptors.requestOrigin?.value, 'request origin'),
      requestPath: requireAbsoluteRequestPath(descriptors.requestPath?.value),
      requestQuery: requireCanonicalRequestQuery(descriptors.requestQuery?.value),
      assertion,
    })
  } catch (cause) {
    assertion.fill(0)
    throw cause
  }
}

function eraseCapturedBootstrap(captured: CapturedGitHubActionsOidcBootstrap): void {
  if (captured.kind === 'request-bootstrap') captured.requestToken.fill(0)
  else captured.assertion.fill(0)
}

export async function requestGithubActionsOidcToken(options: {
  readonly endpoint: URL
  readonly requestToken: Uint8Array
  readonly signal: AbortSignal
}): Promise<Uint8Array> {
  let authorization: string | undefined =
    `Bearer ${new TextDecoder('utf-8', { fatal: true }).decode(options.requestToken)}`
  try {
    return await new Promise<Uint8Array>((resolve, reject) => {
      let settled = false
      let chunks: Buffer[] = []
      let length = 0
      const eraseChunks = (): void => {
        for (const chunk of chunks) chunk.fill(0)
        chunks = []
        length = 0
      }
      const fail = (failure: Error): void => {
        if (settled) return
        settled = true
        eraseChunks()
        reject(failure)
      }
      const request = httpsRequest(options.endpoint, {
        method: 'GET',
        signal: options.signal,
        headers: { Authorization: authorization, Accept: 'application/json' },
        agent: false,
        minVersion: 'TLSv1.3',
      }, (response) => {
        if (response.statusCode !== 200 || response.headers.location !== undefined) {
          fail(new Error('GitHub Actions OIDC authority rejected the request'))
          response.destroy()
          return
        }
        response.on('data', (chunk: Buffer) => {
          if (settled) {
            chunk.fill(0)
            return
          }
          // Incoming stream buffers are adopted directly so the allocation that
          // actually held the assertion JSON is erased, not merely a copy of it.
          const owned = chunk
          length += owned.byteLength
          if (length > MAXIMUM_OIDC_RESPONSE_BYTES) {
            owned.fill(0)
            fail(new Error('GitHub Actions OIDC response exceeded its authority'))
            response.destroy()
            return
          }
          chunks.push(owned)
        })
        response.once('aborted', () => {
          fail(new Error('GitHub Actions OIDC response was aborted'))
        })
        response.once('error', () => {
          fail(new Error('GitHub Actions OIDC response failed'))
        })
        response.once('end', () => {
          if (settled) return
          const bytes = Buffer.concat(chunks, length)
          eraseChunks()
          try {
            const assertion = extractOidcAssertion(bytes)
            settled = true
            resolve(assertion)
          } catch (cause) {
            fail(new Error('GitHub Actions OIDC response is invalid', { cause }))
          } finally {
            bytes.fill(0)
          }
        })
        response.once('close', () => {
          if (!settled && !response.complete) {
            fail(new Error('GitHub Actions OIDC response closed before EOF'))
          }
        })
      })
      request.once('error', () => {
        fail(new Error('GitHub Actions OIDC request failed'))
      })
      request.end()
    })
  } finally {
    authorization = undefined
  }
}

function oidcEndpoint(base: string, audience: string): URL {
  const endpoint = new URL(base)
  endpoint.searchParams.set('audience', audience)
  return endpoint
}

function extractOidcAssertion(bytes: Uint8Array): Uint8Array {
  let offset = skipWhitespace(bytes, 0)
  offset = expectByte(bytes, offset, 0x7b)
  offset = skipWhitespace(bytes, offset)
  for (const byte of [0x22, 0x76, 0x61, 0x6c, 0x75, 0x65, 0x22]) {
    offset = expectByte(bytes, offset, byte)
  }
  offset = skipWhitespace(bytes, offset)
  offset = expectByte(bytes, offset, 0x3a)
  offset = skipWhitespace(bytes, offset)
  offset = expectByte(bytes, offset, 0x22)
  const assertionStart = offset
  while (offset < bytes.byteLength && bytes[offset] !== 0x22) {
    if (!isOidcAssertionByte(bytes[offset] as number)) {
      throw new Error('GitHub Actions OIDC assertion contains a non-canonical byte')
    }
    offset += 1
  }
  const assertion = Buffer.from(bytes.subarray(assertionStart, offset))
  try {
    if (!isOidcAssertion(assertion)) {
      throw new Error('GitHub Actions OIDC assertion is not a compact JWT')
    }
    offset = expectByte(bytes, offset, 0x22)
    offset = skipWhitespace(bytes, offset)
    offset = expectByte(bytes, offset, 0x7d)
    offset = skipWhitespace(bytes, offset)
    if (offset !== bytes.byteLength) {
      throw new Error('GitHub Actions OIDC response has trailing authority')
    }
    return assertion
  } catch (cause) {
    assertion.fill(0)
    throw cause
  }
}

function isOidcAssertion(value: Uint8Array): boolean {
  if (value.byteLength < 5 || value.byteLength > MAXIMUM_OIDC_RESPONSE_BYTES) return false
  let dots = 0
  let segmentBytes = 0
  for (const byte of value) {
    if (byte === 0x2e) {
      if (segmentBytes === 0 || dots === 2) return false
      dots += 1
      segmentBytes = 0
      continue
    }
    if (!isOidcAssertionByte(byte)) return false
    segmentBytes += 1
  }
  return dots === 2 && segmentBytes > 0
}

function isOidcAssertionByte(byte: number): boolean {
  return byte >= 0x30 && byte <= 0x39 || byte >= 0x41 && byte <= 0x5a ||
    byte >= 0x61 && byte <= 0x7a || byte === 0x2d || byte === 0x5f || byte === 0x2e
}

function skipWhitespace(bytes: Uint8Array, start: number): number {
  let offset = start
  while (
    offset < bytes.byteLength &&
    (bytes[offset] === 0x20 || bytes[offset] === 0x09 ||
      bytes[offset] === 0x0a || bytes[offset] === 0x0d)
  ) offset += 1
  return offset
}

function expectByte(bytes: Uint8Array, offset: number, expected: number): number {
  if (offset >= bytes.byteLength || bytes[offset] !== expected) {
    throw new Error('GitHub Actions OIDC response shape is invalid')
  }
  return offset + 1
}

function requireAudience(value: unknown): string {
  if (
    typeof value !== 'string' || value.length < 8 || value.length > 512 ||
    !/^[A-Za-z0-9._:/-]+$/u.test(value)
  ) throw new Error('GitHub Actions OIDC audience is invalid')
  return value
}

function requireCanonicalHttpsOrigin(value: unknown, label: string): string {
  if (typeof value !== 'string') throw new Error(`GitHub Actions OIDC ${label} is invalid`)
  let endpoint: URL
  try {
    endpoint = new URL(value)
  } catch {
    throw new Error(`GitHub Actions OIDC ${label} is invalid`)
  }
  if (
    endpoint.protocol !== 'https:' || endpoint.username !== '' || endpoint.password !== '' ||
    endpoint.pathname !== '/' || endpoint.search !== '' || endpoint.hash !== '' ||
    endpoint.origin !== value
  ) throw new Error(`GitHub Actions OIDC ${label} is invalid`)
  return value
}

function requireAbsoluteRequestPath(value: unknown): string {
  if (
    typeof value !== 'string' || value.length === 0 || value.length > 2_048 ||
    !value.startsWith('/') || value.includes('\0') || value.includes('?') || value.includes('#')
  ) throw new Error('GitHub Actions OIDC request path is invalid')
  const parsed = new URL(value, 'https://authority.invalid')
  if (parsed.pathname !== value) throw new Error('GitHub Actions OIDC request path is invalid')
  return value
}

function requireCanonicalRequestQuery(value: unknown): string {
  if (
    typeof value !== 'string' || value.length === 0 || value.length > 2_048 ||
    !value.startsWith('?') || value.includes('#') || value.includes('\0')
  ) throw new Error('GitHub Actions OIDC request query is invalid')
  const endpoint = new URL(`https://authority.invalid/${value}`)
  if (endpoint.search !== value) throw new Error('GitHub Actions OIDC request query is invalid')
  return value
}

function requireScope(runId: string, profileId: unknown, probeNonce: string): void {
  if (
    !CANONICAL_ID_PATTERN.test(runId) || !OPAQUE_ID_PATTERN.test(probeNonce) ||
    profileId !== 'scheduled-public-stun' && profileId !== 'scheduled-restricted-udp' &&
    profileId !== 'scheduled-coturn'
  ) throw new Error('parent workload identity request scope is invalid')
}

function takeOidcBootstrapEnvironment(environment: NodeJS.ProcessEnv): {
  readonly requestUrl: string
  readonly requestToken: Uint8Array
} {
  const requestUrl = environment.ACTIONS_ID_TOKEN_REQUEST_URL
  const requestTokenValue = environment.ACTIONS_ID_TOKEN_REQUEST_TOKEN
  delete environment.ACTIONS_ID_TOKEN_REQUEST_URL
  delete environment.ACTIONS_ID_TOKEN_REQUEST_TOKEN
  const requestToken = typeof requestTokenValue === 'string'
    ? Buffer.from(requestTokenValue, 'utf8')
    : Buffer.alloc(0)
  if (
    typeof requestUrl !== 'string' || requestUrl.length === 0 || requestUrl.includes('\0') ||
    typeof requestTokenValue !== 'string' || requestTokenValue.length === 0 ||
    requestTokenValue.includes('\0')
  ) {
    requestToken.fill(0)
    throw new Error('GitHub Actions OIDC bootstrap environment is unavailable')
  }
  return Object.freeze({ requestUrl, requestToken })
}
