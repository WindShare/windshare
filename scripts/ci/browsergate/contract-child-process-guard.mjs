import { createRequire, syncBuiltinESMExports } from 'node:module'

const require = createRequire(import.meta.url)
const childProcess = require('node:child_process')
const commands = Object.freeze([
  'exec',
  'execFile',
  'execFileSync',
  'execSync',
  'fork',
  'spawn',
  'spawnSync',
])
const stateKey = Symbol.for('windshare.browser-contract.child-process-guard')
const state = Object.freeze({ commands, invocations: [] })
globalThis[stateKey] = state

for (const command of commands) {
  childProcess[command] = (..._arguments) => {
    state.invocations.push(command)
    throw new Error(`browser-contract attempted forbidden child_process.${command}`)
  }
}
syncBuiltinESMExports()

process.once('beforeExit', () => {
  if (state.invocations.length === 0) return
  process.stderr.write(JSON.stringify({
    component: 'browser-contract-child-process-guard',
    outcome: 'failed',
    invocations: state.invocations,
  }) + '\n')
  process.exitCode = 1
})
