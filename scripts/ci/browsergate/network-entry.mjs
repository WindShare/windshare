import { resolve } from 'node:path'
import { pathToFileURL } from 'node:url'

import {
  NETWORK_MATRIX_IDENTITIES_PER_PROFILE,
  NETWORK_MATRIX_IDENTITY_COUNTS,
  NETWORK_MATRIX_PROFILE_REGISTRY,
  NETWORK_MATRIX_REPORTING_SEMANTICS,
} from '../../../web/scripts/browser-network-matrix/vocabulary.ts'

const LOCAL_NETWORK_ENTRY_SCHEMA = 'windshare.browser-network-matrix.local-entry/v1'

const UNPROVISIONED_PROFILE_RESULTS = Object.freeze(NETWORK_MATRIX_PROFILE_REGISTRY.map((profile) =>
  Object.freeze({
    profileId: profile.profileId,
    executionMode: profile.executionMode,
    authorityId: profile.authorityId,
    prerequisiteOutcome: 'unavailable',
    failureCode: 'authority-not-provisioned',
    expectedSamples: NETWORK_MATRIX_IDENTITIES_PER_PROFILE,
    observedSamples: 0,
    profileOutcome: 'not-executed',
  })))

const NO_CONFIG_NETWORK_EVIDENCE = Object.freeze({
  schemaVersion: LOCAL_NETWORK_ENTRY_SCHEMA,
  component: 'browser-network-entry',
  reportingSemantics: NETWORK_MATRIX_REPORTING_SEMANTICS,
  outcome: 'not-executed',
  blocking: false,
  reason: 'external-authorities-not-provisioned',
  counts: Object.freeze({
    expectedSamples: NETWORK_MATRIX_IDENTITY_COUNTS.total,
    observedSamples: 0,
  }),
  profileResults: UNPROVISIONED_PROFILE_RESULTS,
  nextStep:
    'Build helpers into an explicit new absolute directory, then pass an explicit execute or aggregate command.',
})

export async function runBrowserNetworkEntry(
  arguments_,
  composition = {},
) {
  if (!Array.isArray(arguments_) || arguments_.some((argument) => typeof argument !== 'string')) {
    throw new Error('browser network entry arguments must be strings')
  }
  const write = composition.write ?? ((encoded) => process.stdout.write(encoded))
  if (arguments_.length === 0) {
    // Absence is evidence only when no authority boundary is crossed. Keeping
    // CLI resolution below this branch prevents OIDC, broker, helper, fixture,
    // and filesystem authorities from being loaded for a dormant local run.
    write(`${JSON.stringify(NO_CONFIG_NETWORK_EVIDENCE)}\n`)
    return 0
  }
  // The matrix CLI requires explicit manifest, helper-manifest, publisher, and
  // Windows Job paths. Forwarding the exact operands preserves that authority;
  // this gate never infers a helper or silently provisions a topology.
  const runMatrixCli = composition.runMatrixCli ?? defaultMatrixCli
  return runMatrixCli(Object.freeze([...arguments_]))
}

async function defaultMatrixCli(arguments_) {
  const { browserNetworkMatrixCli } = await import(
    '../../../web/scripts/browser-network-matrix/cli/main.ts'
  )
  return browserNetworkMatrixCli(arguments_)
}

const invokedPath = process.argv[1]
if (invokedPath !== undefined && pathToFileURL(resolve(invokedPath)).href === import.meta.url) {
  try {
    process.exitCode = await runBrowserNetworkEntry(process.argv.slice(2))
  } catch (cause) {
    process.stderr.write(`${JSON.stringify({
      schemaVersion: LOCAL_NETWORK_ENTRY_SCHEMA,
      component: 'browser-network-entry',
      outcome: 'failed',
      blocking: false,
      error: cause instanceof Error ? cause.message : String(cause),
    })}\n`)
    process.exitCode = 1
  }
}
