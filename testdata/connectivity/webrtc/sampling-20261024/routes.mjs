import assert from 'node:assert/strict'

function replace(body, before, after) {
  assert.equal(body.split(before).length, 2, 'Sampling study anchor: ' + before)
  return body.replace(before, after)
}

export async function installRoutes(page, config) {
  await page.route('**/src/connectivity/v2-receiver-policy.ts', async route => {
    const response = await route.fetch()
    let body = await response.text()
    const anchor = 'const demand = this.#lanes.schedulingDemand();'
    body = replace(body, anchor, anchor + '\n(window.__bottleneckDemand ??= []).push({at: performance.now(), ...demand, peers: this.#installedPeers.size});')
    const apply = 'await this.#applyParallelism(decision);'
    if (config.paths) {
      body = replace(body, 'await clock.sleep(THROUGHPUT_SAMPLE_MS, this.#lifetime.signal);', 'await clock.sleep(100, this.#lifetime.signal);')
      body = replace(body, apply, `const count = Array.isArray(paths) ? paths.length : paths;
        if (!pending && count > 0 && count < ${config.paths} && this.#activations.size > 0) this.#openOptional();`)
    } else if (config.fault !== 'none') {
      assert(['retire', 'slow'].includes(config.fault))
      body = replace(body, apply, apply + `
        if (decision.action === 'sample' && !window.__samplingFault) {
          const peer = this.#installedPeers.get(decision.path.pathId);
          if (peer) {
            window.__samplingFault = { at: performance.now(), kind: ${JSON.stringify(config.fault)}, ...decision.path };
            ${config.fault === 'retire' ? 'setTimeout(() => peer.peer.close(), 250);' : ''}
          }
        }`)
    }
    await route.fulfill({ response, body })
  })
  if (config.fault === 'slow') await page.route('**/src/content/v2-session-services.ts', async route => {
    const response = await route.fetch()
    let body = await response.text()
    const anchor = 'async fetchBlock(demand, signal, observer) {'
    body = replace(body, anchor, anchor + `
      if (window.__samplingFault?.laneId === this.id) window.__samplingSlowedLane ??= this;
      if (window.__samplingSlowedLane === this) await new Promise((resolve, reject) => {
        const abort = () => { clearTimeout(timer); reject(signal.reason); };
        const timer = setTimeout(() => { signal.removeEventListener('abort', abort); resolve(); }, 600);
        signal.addEventListener('abort', abort, { once: true });
        if (signal.aborted) abort();
      });`)
    await route.fulfill({ response, body })
  })
}
