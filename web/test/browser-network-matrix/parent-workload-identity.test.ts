import { describe, expect, it, vi } from 'vitest'

import {
  GitHubActionsOidcBootstrapLease,
  GitHubActionsOidcIdentityAuthority,
  MINTED_GITHUB_ACTIONS_OIDC_PROTOCOL,
  PARENT_WORKLOAD_IDENTITY_PROTOCOL,
} from '../../scripts/browser-network-matrix/linux-topology/parent-workload-identity.ts'

const REQUEST_ORIGIN = 'https://pipelines.actions.githubusercontent.com'
const REQUEST_PATH = '/oidc/token'
const REQUEST_URL = `${REQUEST_ORIGIN}${REQUEST_PATH}?api-version=2.0`
const ISSUER = 'https://token.actions.githubusercontent.com'
const AUDIENCE = 'windshare-browser-network-matrix'
const REPOSITORY = 'windshare/windshare'
const REF = 'refs/heads/main'
const WORKFLOW_REF = 'windshare/windshare/.github/workflows/browser-network-matrix.yml@refs/heads/main'
const BOOTSTRAP_TOKEN = 'runner-bootstrap-token'

describe('GitHub Actions parent workload identity authority', () => {
  it('binds one copied minted assertion to the exact audience and erases it on lease close', async () => {
    const sourceAssertion = Buffer.from('header.payload.signature', 'ascii')
    const lease = GitHubActionsOidcBootstrapLease.fromMintedEnvelope({
      protocolVersion: MINTED_GITHUB_ACTIONS_OIDC_PROTOCOL,
      audience: AUDIENCE,
      requestOrigin: REQUEST_ORIGIN,
      requestPath: REQUEST_PATH,
      requestQuery: '?api-version=2.0',
      assertion: sourceAssertion,
    })
    const authority = lease.consume(authorityOptions())

    const issued = await authority.issue(issueScope())
    expect(Buffer.from(issued).toString('ascii')).toBe('header.payload.signature')
    issued.fill(0)
    await lease.closeAndWait()

    await expect(authority.issue(issueScope())).rejects.toThrow(
      'parent workload identity request was terminated',
    )
    expect(Buffer.from(sourceAssertion).toString('ascii')).toBe('header.payload.signature')
    sourceAssertion.fill(0)
  })

  it('rejects a minted assertion whose audience does not match the sealed runtime binding', () => {
    const lease = GitHubActionsOidcBootstrapLease.fromMintedEnvelope({
      protocolVersion: MINTED_GITHUB_ACTIONS_OIDC_PROTOCOL,
      audience: 'different-browser-network-audience',
      requestOrigin: REQUEST_ORIGIN,
      requestPath: REQUEST_PATH,
      requestQuery: '?api-version=2.0',
      assertion: Buffer.from('header.payload.signature', 'ascii'),
    })

    expect(() => lease.consume(authorityOptions())).toThrow(
      'minted GitHub Actions OIDC endpoint binding is invalid',
    )
  })

  it('rejects active and proxied minted envelopes without invoking hostile hooks', () => {
    let getterCalls = 0
    const active = {
      protocolVersion: MINTED_GITHUB_ACTIONS_OIDC_PROTOCOL,
      audience: AUDIENCE,
      requestOrigin: REQUEST_ORIGIN,
      requestPath: REQUEST_PATH,
      requestQuery: '?api-version=2.0',
      get assertion() {
        getterCalls += 1
        return Buffer.from('header.payload.signature', 'ascii')
      },
    }
    expect(() => GitHubActionsOidcBootstrapLease.fromMintedEnvelope(active)).toThrow(
      'minted GitHub Actions OIDC envelope is active',
    )
    expect(getterCalls).toBe(0)

    let trapCalls = 0
    const proxied = new Proxy(active, {
      ownKeys() {
        trapCalls += 1
        return []
      },
    })
    expect(() => GitHubActionsOidcBootstrapLease.fromMintedEnvelope(proxied)).toThrow(
      'minted GitHub Actions OIDC envelope is invalid',
    )
    expect(trapCalls).toBe(0)
  })

  it('removes runner bootstrap environment, pins its authority, and zeroes on close', async () => {
    const environment: NodeJS.ProcessEnv = {
      ACTIONS_ID_TOKEN_REQUEST_URL: REQUEST_URL,
      ACTIONS_ID_TOKEN_REQUEST_TOKEN: BOOTSTRAP_TOKEN,
    }
    let observedBootstrap: Uint8Array | undefined
    const request = vi.fn(async (input: {
      readonly endpoint: URL
      readonly requestToken: Uint8Array
      readonly signal: AbortSignal
    }) => {
      observedBootstrap = input.requestToken
      expect(input.endpoint.toString()).toBe(`${REQUEST_URL}&audience=${AUDIENCE}`)
      return Buffer.from('header.payload.signature', 'ascii')
    })
    const authority = GitHubActionsOidcIdentityAuthority.createTestHarness({
      ...authorityOptions(),
      environment,
      request,
    })

    expect(environment).not.toHaveProperty('ACTIONS_ID_TOKEN_REQUEST_URL')
    expect(environment).not.toHaveProperty('ACTIONS_ID_TOKEN_REQUEST_TOKEN')
    expect(authority.binding).toEqual({
      protocolVersion: PARENT_WORKLOAD_IDENTITY_PROTOCOL,
      kind: 'github-actions-oidc',
      audience: AUDIENCE,
      issuer: ISSUER,
      repository: REPOSITORY,
      ref: REF,
      workflowRef: WORKFLOW_REF,
      requestOrigin: REQUEST_ORIGIN,
      requestPath: REQUEST_PATH,
      requestQuery: '?api-version=2.0',
    })
    const assertion = await authority.issue(issueScope())
    expect(Buffer.from(assertion).toString('ascii')).toBe('header.payload.signature')
    expect(Buffer.from(observedBootstrap as Uint8Array).toString('utf8')).toBe(BOOTSTRAP_TOKEN)

    await authority.closeAndWait()

    expect([...observedBootstrap as Uint8Array]).toEqual(
      new Array(BOOTSTRAP_TOKEN.length).fill(0),
    )
    await expect(authority.issue(issueScope())).rejects.toThrow(
      'parent workload identity request was terminated',
    )
    assertion.fill(0)
  })

  it('rejects a runner endpoint outside the sealed origin/path and removes both sentinels', () => {
    const environment: NodeJS.ProcessEnv = {
      ACTIONS_ID_TOKEN_REQUEST_URL: 'https://attacker.invalid/collect',
      ACTIONS_ID_TOKEN_REQUEST_TOKEN: BOOTSTRAP_TOKEN,
    }

    expect(() => GitHubActionsOidcIdentityAuthority.createTestHarness({
      ...authorityOptions(),
      environment,
      request: vi.fn(),
    })).toThrow('GitHub Actions OIDC endpoint is invalid')
    expect(environment).not.toHaveProperty('ACTIONS_ID_TOKEN_REQUEST_URL')
    expect(environment).not.toHaveProperty('ACTIONS_ID_TOKEN_REQUEST_TOKEN')
  })

  it('atomically removes a token even when the runner URL is missing', () => {
    const environment: NodeJS.ProcessEnv = {
      ACTIONS_ID_TOKEN_REQUEST_TOKEN: BOOTSTRAP_TOKEN,
    }

    expect(() => GitHubActionsOidcIdentityAuthority.createTestHarness({
      ...authorityOptions(),
      environment,
      request: vi.fn(),
    })).toThrow('GitHub Actions OIDC bootstrap environment is unavailable')
    expect(environment).not.toHaveProperty('ACTIONS_ID_TOKEN_REQUEST_URL')
    expect(environment).not.toHaveProperty('ACTIONS_ID_TOKEN_REQUEST_TOKEN')
  })

  it('removes both sentinels before rejecting invalid sealed claim options', () => {
    const environment: NodeJS.ProcessEnv = {
      ACTIONS_ID_TOKEN_REQUEST_URL: REQUEST_URL,
      ACTIONS_ID_TOKEN_REQUEST_TOKEN: BOOTSTRAP_TOKEN,
    }

    expect(() => GitHubActionsOidcIdentityAuthority.createTestHarness({
      ...authorityOptions(),
      audience: 'bad audience',
      environment,
      request: vi.fn(),
    })).toThrow('GitHub Actions OIDC audience is invalid')
    expect(environment).not.toHaveProperty('ACTIONS_ID_TOKEN_REQUEST_URL')
    expect(environment).not.toHaveProperty('ACTIONS_ID_TOKEN_REQUEST_TOKEN')
  })

  it('zeroes a malformed issued assertion before rejecting it', async () => {
    let malformed: Uint8Array | undefined
    const authority = GitHubActionsOidcIdentityAuthority.createTestHarness({
      ...authorityOptions(),
      environment: {
        ACTIONS_ID_TOKEN_REQUEST_URL: REQUEST_URL,
        ACTIONS_ID_TOKEN_REQUEST_TOKEN: BOOTSTRAP_TOKEN,
      },
      request: vi.fn(async () => {
        malformed = Buffer.from('not-a-jwt', 'ascii')
        return malformed
      }),
    })

    await expect(authority.issue(issueScope())).rejects.toThrow(
      'GitHub Actions OIDC assertion is invalid',
    )
    expect([...malformed as Uint8Array]).toEqual(new Array('not-a-jwt'.length).fill(0))
    await authority.closeAndWait()
  })

  it('aborts and waits an in-flight issue before publishing one idempotent close receipt', async () => {
    let markStarted: (() => void) | undefined
    const started = new Promise<void>((resolve) => { markStarted = resolve })
    let bootstrap: Uint8Array | undefined
    let lateAssertion: Uint8Array | undefined
    const authority = GitHubActionsOidcIdentityAuthority.createTestHarness({
      ...authorityOptions(),
      environment: {
        ACTIONS_ID_TOKEN_REQUEST_URL: REQUEST_URL,
        ACTIONS_ID_TOKEN_REQUEST_TOKEN: BOOTSTRAP_TOKEN,
      },
      request: vi.fn(async ({ requestToken, signal }) => {
        bootstrap = requestToken
        markStarted?.()
        await new Promise<void>((resolve) => {
          if (signal.aborted) resolve()
          else signal.addEventListener('abort', () => resolve(), { once: true })
        })
        lateAssertion = Buffer.from('header.payload.signature', 'ascii')
        return lateAssertion
      }),
    })
    const issuance = authority.issue(issueScope())
    const issuanceFailure = expect(issuance).rejects.toThrow(
      'parent workload identity request was terminated',
    )
    await started

    const receipts = await Promise.all([
      authority.closeAndWait(),
      authority.closeAndWait(),
      authority.forceTerminateAndWait(),
    ])

    await issuanceFailure
    expect(receipts).toEqual([
      { terminal: 'closed' }, { terminal: 'closed' }, { terminal: 'closed' },
    ])
    expect([...lateAssertion as Uint8Array]).toEqual(
      new Array('header.payload.signature'.length).fill(0),
    )
    expect([...bootstrap as Uint8Array]).toEqual(new Array(BOOTSTRAP_TOKEN.length).fill(0))
  })
})

function authorityOptions() {
  return {
    audience: AUDIENCE,
    issuer: ISSUER,
    repository: REPOSITORY,
    ref: REF,
    workflowRef: WORKFLOW_REF,
    requestOrigin: REQUEST_ORIGIN,
    requestPath: REQUEST_PATH,
    requestQuery: '?api-version=2.0',
  } as const
}

function issueScope() {
  return {
    runId: 'network-matrix-run',
    profileId: 'scheduled-public-stun' as const,
    probeNonce: 'probeNonce0123456789',
    signal: new AbortController().signal,
  }
}
