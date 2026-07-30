import assert from 'node:assert/strict'

const state = globalThis[Symbol.for('windshare.browser-contract.child-process-guard')]
assert.notEqual(state, undefined, 'browser-contract child-process guard preload is absent')
assert.deepEqual(state.commands, [
  'exec',
  'execFile',
  'execFileSync',
  'execSync',
  'fork',
  'spawn',
  'spawnSync',
])
assert.deepEqual(state.invocations, [])
