import assert from 'node:assert/strict'

import { suiteExecutionPlan } from './main.mjs'
import { verifyPlaywrightDiscoveryIntegration } from './playwright-discovery.tests.mjs'

const [suite, ...unexpected] = process.argv.slice(2)
assert.equal(unexpected.length, 0, 'Playwright discovery integration accepts exactly one suite')
verifyPlaywrightDiscoveryIntegration(suiteExecutionPlan, suite)
process.stdout.write(`browsergate ${suite} Playwright discovery integration: PASS\n`)
