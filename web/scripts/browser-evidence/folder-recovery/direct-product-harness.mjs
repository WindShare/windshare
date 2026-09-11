import { runCase as runProductCase, cleanupCase } from './product-harness.mjs'

/** Existing FSA checkpoint and ledger cost stays in the baseline for the automatic-folder comparison. */
export function runCase(input) {
  return runProductCase({ ...input, mode: 'direct', bypassDelivery: true })
}
export { cleanupCase }
