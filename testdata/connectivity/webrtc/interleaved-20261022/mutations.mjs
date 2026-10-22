import assert from 'node:assert/strict'

export async function installRoutes(page, { arm, counters }) {
  if (arm === 'reserve-zero') {
    await page.route('**/src/content/scheduling/range-window.ts', async route => {
      const response = await route.fetch(), original = await response.text()
      const needle = 'Math.min(interactive, Math.floor(maximum / 2))'
      assert.equal(original.split(needle).length, 2)
      await route.fulfill({ response, body: original.replace(needle, '0') })
    })
  }
  if (arm === 'readiness-wake') {
    await page.route('**/src/content/scheduling/receive-window.ts', async route => {
      const response = await route.fetch()
      let body = await response.text()
      const clock = /const now = this\.#now\(\);?/g
      assert.equal([...body.matchAll(clock)].length, 1)
      body = body.replace(clock, 'const wasAvailable = this.available; $&')
      const notification = /this\.#changed\(\);?/g
      assert.equal([...body.matchAll(notification)].length, 2)
      // Acquisition itself never wakes waiters. Receipt need only announce that
      // previously blocked lane capacity has become available; close still wakes.
      body = body.replace(/this\.#changed\(\);?/, 'if (!wasAvailable && this.available) this.#changed();')
      await route.fulfill({ response, body })
    })
  }
  if (counters && arm !== 'old') {
    await page.route('**/src/content/scheduling/admission.ts', async route => {
      const response = await route.fetch()
      let body = await response.text()
      body = 'const retestCounters = window.__retestAdmissionCounters = { drains: 0, selects: 0, commits: 0 };\n' + body
      for (const [method, counter] of [['drain','drains'],['select','selects'],['commit','commits']]) {
        const pattern = new RegExp('#' + method + '\\([^)]*\\)\\s*\\{', 'g')
        assert.equal([...body.matchAll(pattern)].length, 1, method)
        body = body.replace(pattern, match => match + ' retestCounters.' + counter + '++;')
      }
      await route.fulfill({ response, body })
    })
  }
}
