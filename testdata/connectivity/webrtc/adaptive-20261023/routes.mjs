import assert from 'node:assert/strict'

export async function installRoutes(page, config) {
  await page.route('**/src/connectivity/v2-receiver-policy.ts', async route => {
    const response = await route.fetch()
    let body = await response.text()
    if (config.turnOnly) {
      // Constrain the study's content route without replacing negotiation,
      // credential delegation, recovery, scheduling, or the output pipeline.
      const eligibility = /return this\.#active && \([\s\S]+?;\n/g
      assert.equal([...body.matchAll(eligibility)].length, 1)
      body = body.replace(eligibility, 'return this.#active && route === "turn";\n')
    }
    if (config.arm === 'single') {
      assert(body.includes('PEER_CONTINUATION_SLOTS - 1'))
      body = body.replace('PEER_CONTINUATION_SLOTS - 1', '1')
    }
    const anchor = 'const demand = this.#lanes.schedulingDemand();'
    assert(body.includes(anchor))
    body = body.replace(anchor, anchor + `
      (window.__adaptiveDemand ??= []).push({ at: performance.now(), ...demand, peers: this.#installedPeers.size });
      if (${JSON.stringify(config.fault === 'retire')} && this.#optionalPaths.size > 0 && !window.__adaptiveFault) {
        const entry = [...this.#optionalPaths.values()].at(-1);
        const installed = this.#installedPeers.get(encodeBase64Url(entry.identity.copyBytes()));
        if (installed) {
          window.__adaptiveFault = { at: performance.now(), laneId: installed.laneId };
          await installed.peer.close();
        }
      }
    `)
    await route.fulfill({ response, body })
  })
}
