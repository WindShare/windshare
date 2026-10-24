import assert from 'node:assert/strict'

function replace(body, before, after) {
  assert.equal(body.split(before).length, 2, 'Research overlay anchor: ' + before)
  return body.replace(before, after)
}

// Fixed-path controls change the experiment's policy, while production
// negotiation, admission, scheduling, authentication and output remain active.
export async function installRoutes(page, config) {
  await page.route('**/src/connectivity/v2-receiver-policy.ts', async route => {
    const response = await route.fetch()
    let body = await response.text()
    const anchor = 'const demand = this.#lanes.schedulingDemand();'
    body = replace(body, anchor, anchor + '\n(window.__bottleneckDemand ??= []).push({at: performance.now(), ...demand, peers: this.#installedPeers.size});')
    if (config.paths) {
      body = replace(body, 'await clock.sleep(THROUGHPUT_SAMPLE_MS, this.#lifetime.signal);', 'await clock.sleep(100, this.#lifetime.signal);')
      body = replace(body, 'await this.#applyParallelism(decision);', `if (!pending && paths > 0 && paths < ${config.paths} && this.#activations.size > 0) this.#openOptional();`)
    }
    await route.fulfill({ response, body })
  })
  if (['success-no-cooldown', 'recovery-window'].includes(config.policy)) {
    await page.route('**/src/connectivity/peer-set/throughput.ts', async route => {
      const response = await route.fetch()
      let body = await response.text()
      body = replace(body, 'action: improved ? "keep" : "retire",', 'action: improved ? (this.#notBefore = now, "keep") : "retire",')
      if (config.policy === 'recovery-window') body = replace(body, 'const SETTLING_MS = 1e3;', 'const SETTLING_MS = 5e3;')
      await route.fulfill({ response, body })
    })
  }
}
