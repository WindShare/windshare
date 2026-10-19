import { expect, type APIRequestContext } from '@playwright/test'

const CONTROL_TIMEOUT_MILLISECONDS = 15_000

interface PreviewStatus {
  readonly boot: string
  readonly sockets: readonly { readonly state: number }[]
}

/** Fault authority stays in the test runner and never enters the browser capability. */
export class PreviewControl {
  readonly #request: APIRequestContext
  readonly #base: string
  readonly #secret: string
  readonly #shareId: string
  #resetSent = false

  constructor(request: APIRequestContext, base: string, secret: string, shareId: string) {
    this.#request = request
    this.#base = base
    this.#secret = secret
    this.#shareId = shareId
  }

  async status(): Promise<PreviewStatus> {
    const response = await this.#request.get(this.#url('status'), this.#options())
    expect(response.status(), 'Preview status must be authenticated and available').toBe(200)
    const status = await response.json() as PreviewStatus
    expect(status.boot).toEqual(expect.any(String))
    expect(status.boot.length).toBeGreaterThan(0)
    return status
  }

  async resetOnce(before: PreviewStatus): Promise<{ beforeBoot: string; afterBoot: string; response: number | 'transport-error' }> {
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
    return { beforeBoot: before.boot, afterBoot: after.boot, response }
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
      headers: { Authorization: 'Bearer ' + this.#secret },
      timeout: CONTROL_TIMEOUT_MILLISECONDS,
      maxRetries: 0,
      maxRedirects: 0,
    }
  }
}
