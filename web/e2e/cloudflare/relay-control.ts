import { expect, type APIRequestContext } from '@playwright/test'

const CONTROL_TIMEOUT_MILLISECONDS = 30_000
const DEPLOYMENT_TIMEOUT_MILLISECONDS = 360_000

export interface RelayStatus {
  readonly boot: string
  readonly versionId: string | null
  readonly workerVersionId: string
  readonly now: number
  readonly revision: string | null
  readonly storageMarker: string
  readonly accessCredentialAccepted: boolean
  readonly sockets: readonly { readonly state: number }[]
}

/** Test authority stays in the runner and never enters the browser capability. */
export class RelayTestControl {
  readonly #request: APIRequestContext
  readonly #base: string
  readonly #secret: string
  readonly #accessKey: string
  readonly #shareId: string
  #resetSent = false
  lastStatus: RelayStatus | undefined

  constructor(request: APIRequestContext, base: string, secret: string, accessKey: string, shareId: string) {
    this.#request = request
    this.#base = base
    this.#secret = secret
    this.#accessKey = accessKey
    this.#shareId = shareId
  }

  async status(): Promise<RelayStatus> {
    const response = await this.#request.get(this.#url('status'), this.#options())
    expect(response.status(), 'Test status must be authenticated and available').toBe(200)
    const status = { ...await response.json() as RelayStatus, workerVersionId: response.headers()['x-test-worker-version'] ?? '' }
    this.lastStatus = status
    expect(status.boot).toEqual(expect.any(String))
    expect(status.boot.length).toBeGreaterThan(0)
    expect(status.storageMarker).toEqual(expect.any(String))
    expect(status.storageMarker.length).toBeGreaterThan(0)
    expect(status.accessCredentialAccepted, 'Updates must retain the registration credential').toBe(true)
    return status
  }

  async waitForVersion(versionId: string): Promise<RelayStatus> {
    let status: RelayStatus | undefined
    let previous = ''
    await expect.poll(async () => {
      status = await this.status()
      const observed = status.workerVersionId + '/' + status.versionId
      if (observed !== previous) {
        console.info(JSON.stringify({ event: 'cloudflare-version-observed', expectedVersionId: versionId, status }))
        previous = observed
      }
      return status.versionId
    }, { timeout: DEPLOYMENT_TIMEOUT_MILLISECONDS }).toBe(versionId)
    return status!
  }

  async resetOnce(before: RelayStatus) {
    if (this.#resetSent) throw new Error('This scenario permits exactly one reset request')
    this.#resetSent = true
    // abort() deliberately fails its HTTP request. Neither a 5xx nor a network
    // error proves a reset; constructor identity and socket loss do.
    const response = await this.#request.post(this.#url('reset'), this.#options())
      .then(result => result.status(), () => 'transport-error' as const)
    if (typeof response === 'number') expect(response).toBeGreaterThanOrEqual(500)
    let after = before
    await expect.poll(async () => {
      after = await this.status()
      return after.boot
    }, { timeout: CONTROL_TIMEOUT_MILLISECONDS }).not.toBe(before.boot)
    expect(after.storageMarker).toBe(before.storageMarker)
    return { before, after, response }
  }

  #url(operation: 'status' | 'reset'): string {
    const url = new URL('/__test/' + operation, this.#base)
    if (url.protocol === 'wss:') url.protocol = 'https:'
    if (url.protocol === 'ws:') url.protocol = 'http:'
    url.searchParams.set('share_id', this.#shareId)
    return url.href
  }

  #options() {
    return {
      headers: {
        Authorization: 'Bearer ' + this.#secret,
        'X-Test-Access-Key': this.#accessKey,
      },
      timeout: CONTROL_TIMEOUT_MILLISECONDS,
      maxRetries: 0,
      maxRedirects: 0,
    }
  }
}
