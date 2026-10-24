import assert from 'node:assert/strict'

function replaceOnce(body, pattern, replacement) {
  const matches = typeof pattern === 'string' ? body.split(pattern).length - 1 : [...body.matchAll(new RegExp(pattern.source, 'g'))].length
  assert.equal(matches, 1, 'Research overlay anchor: ' + pattern)
  return body.replace(pattern, replacement)
}

// These changes exist only in intercepted laboratory pages. The production
// admission, provider, authentication and allocation paths still own the work.
export async function installRoutes(page, config) {
  await page.route('**/src/connectivity/v2-receiver-policy.ts', async route => {
    const response = await route.fetch()
    let body = await response.text()
    body = replaceOnce(body, /class V2ReceiverConnectivity\s*\{/, match => match + '\n #researchExtras = []; #researchExposed = false;')
    body = replaceOnce(body, /return path\.activate\(\);?/, `
      const activation = path.activate();
      if (!this.#researchExposed) {
        this.#researchExposed = true;
        const events = [];
        const grow = async count => {
          const deadline = performance.now() + 20000;
          while (this.#researchExtras.length < count - 1) {
            if (${JSON.stringify(config.launch === 'serial')}) {
              while (this.#installedPeers.size < this.#researchExtras.length + 1) {
                if (performance.now() > deadline) throw new Error('Serial research launch timed out');
                await new Promise(resolve => setTimeout(resolve, 20));
              }
            }
            const id = createV2PeerPathIdentityValue(createV2PeerPathIdentity());
            const owner = this.#createPeerRecovery(id, this.#offers, () => {
              events.push({ kind: 'unavailable', at: performance.now() });
              this.#peerUnavailable();
            });
            const lease = owner.activate();
            this.#researchExtras.push({ id, lease });
            events.push({ kind: 'created', at: performance.now(), count: this.#researchExtras.length + 1 });
          }
        };
        window.__preflightControl = {
          grow, events,
          failOptional: () => this.#peerUnavailable(),
          state: () => ({ admitted: this.#admitted.size, directPeers: this.#installedPeers.size, activations: this.#activations.size }),
          retireLast: async () => {
            const entry = this.#researchExtras.pop();
            if (!entry) return;
            entry.lease.close();
            const installed = this.#installedPeers.get(encodeBase64Url(entry.id.copyBytes()));
            if (installed) this.#releasePeer(installed.laneId, installed.laneEpoch);
            await this.#peers.remove(entry.id);
            events.push({ kind: 'retired', at: performance.now() });
          },
        };
        if (!${JSON.stringify(config.late)}) grow(${config.connections}).catch(error => events.push({ kind: 'launch-failed', error: String(error) }));
      }
      return activation;
    `)
    await route.fulfill({ response, body })
  })

  await page.route('**/src/content/scheduling/block-buffers.ts', async route => {
    const response = await route.fetch()
    let body = await response.text()
    body = 'const researchBuffers = window.__preflightBuffers = { peakReserved: 0, peakCache: 0, reserved: 0, cached: 0 };\n' + body
    body = replaceOnce(body, /this\.#reservedBytes \+= needed\(\);?/, match => match + ' researchBuffers.reserved = this.#reservedBytes; researchBuffers.peakReserved = Math.max(researchBuffers.peakReserved, this.#reservedBytes);')
    body = replaceOnce(body, /this\.#reservedBytes -= bytes;?/, match => match + ' researchBuffers.reserved = this.#reservedBytes;')
    body = replaceOnce(body, /this\.#cachedBytes \+= bytes;?/, match => match + ' researchBuffers.cached = this.#cachedBytes; researchBuffers.peakCache = Math.max(researchBuffers.peakCache, this.#cachedBytes);')
    await route.fulfill({ response, body })
  })
}
