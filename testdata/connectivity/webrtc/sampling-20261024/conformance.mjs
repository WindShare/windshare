import assert from 'node:assert/strict'
import fs from 'node:fs'
import { ThroughputController } from '../../../../web/src/connectivity/peer-set/throughput.ts'

const inputs = JSON.parse(fs.readFileSync('.tmp/sampling-stress-tools/generated.json', 'utf8'))
const expected = JSON.parse(fs.readFileSync('.tmp/sampling-stress-tools/native.json', 'utf8'))
function canonical(value) {
  if (Array.isArray(value)) return value.map(canonical)
  if (value && typeof value === 'object') return Object.fromEntries(Object.entries(value).map(([key, child]) => [key[0].toLowerCase() + key.slice(1).replaceAll('ID', 'Id'), canonical(child)]))
  return value
}
let decisions = 0
for (let index = 0; index < inputs.length; index++) {
  const controller = new ThroughputController()
  const scenario = inputs[index]
  for (let n = 0; n < scenario.samples.length; n++) {
    const sample = scenario.samples[n]
    const actual = controller.step(sample.at, sample, scenario.maximumPaths)
    const native = canonical(expected[index][n])
    const location = `${index}:${n} at ${sample.at}`
    assert.equal(actual.action, native.action, location)
    assert.equal(actual.reason ?? '', native.reason, location)
    assert.deepEqual(actual.path ?? { pathId: '', laneId: 0, laneEpoch: 0 }, native.path, location)
    if (actual.work) assert.deepEqual(actual.work, native.work, location)
    for (const field of ['usefulRate','outputRate','baselineUsefulRate','baselineOutputRate']) {
      const actualRate = actual[field] ?? 0
      assert(Number.isFinite(actualRate) && actualRate >= 0, location)
      assert(Math.abs(actualRate - native[field]) <= Math.max(1, actualRate) * 1e-9, location + ':' + field)
    }
    decisions++
  }
}
console.log(JSON.stringify({scenarios: inputs.length, decisions, status:'passed'}))
