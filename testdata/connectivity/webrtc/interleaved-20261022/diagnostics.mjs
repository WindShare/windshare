import assert from 'node:assert/strict'

export function instrument(source) {
  function replace(before, after) {
    assert.equal(source.split(before).length, 2, 'Diagnostic contract: ' + before)
    source = source.replace(before, after)
  }
  replace('const modules = await loadProductModules()', `const modules = await loadProductModules()
        const blockTimings = []
        const { V2SessionBlockLane } = await import('/src/content/v2-session-services.ts')
        const originalFetchBlock = V2SessionBlockLane.prototype.fetchBlock
        V2SessionBlockLane.prototype.fetchBlock = async function(demand, signal, observer) {
          const timing = { index: String(demand.localBlockIndex), started: performance.now(), receivedBytes: 0 }
          blockTimings.push(timing)
          try {
            const record = await originalFetchBlock.call(this, demand, signal, {
              ...observer,
              received: bytes => {
                const now = performance.now()
                timing.firstReceived ??= now
                timing.lastReceived = now
                timing.receivedBytes += bytes
                observer?.received(bytes)
              },
            })
            timing.authenticated = performance.now()
            return record
          } catch (error) {
            timing.failed = String(error)
            throw error
          }
        }`)
  replace('started, timeOrigin: performance.timeOrigin, receiveTimeline,', 'started, timeOrigin: performance.timeOrigin, blockTimings, receiveTimeline,')
  replace("transferMetrics[phase] = { metrics: await cdp.send('Performance.getMetrics'), processes: await browserCDP.send('SystemInfo.getProcessInfo') }",
    `transferMetrics[phase] = { metrics: await cdp.send('Performance.getMetrics'), processes: await browserCDP.send('SystemInfo.getProcessInfo') }
      if (phase === 'before') {
        await cdp.send('Profiler.enable')
        await cdp.send('Profiler.setSamplingInterval', { interval: 1000 })
        await cdp.send('Profiler.start')
      } else {
        const profile = await cdp.send('Profiler.stop')
        await writeFile(join(output, n + '-cpu-profile.json'), JSON.stringify(profile))
      }`)
  return source
}
