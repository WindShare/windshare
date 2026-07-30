import assert from 'node:assert/strict'
import { resolve } from 'node:path'

import {
  assertPinnedNodeVersion,
  parsePinnedNodeVersion,
  readPinnedNodeVersion,
} from '../../../node-version.mjs'

const REPOSITORY_ROOT = resolve(import.meta.dirname, '..', '..', '..', '..', '..')
const PINNED_NODE_VERSION = '24.16.0'

for (const source of [
  PINNED_NODE_VERSION,
  `${PINNED_NODE_VERSION}\n`,
  `${PINNED_NODE_VERSION}\r\n`,
]) assert.equal(parsePinnedNodeVersion(source), PINNED_NODE_VERSION)

for (const source of [
  '',
  'v24.16.0',
  '24',
  '24.16',
  '24.16.0.1',
  '24.x.0',
  '^24.16.0',
  '024.16.0',
  '24.016.0',
  '24.16.00',
  '24.16.0 ',
  ' 24.16.0',
  '24.16.0\r',
  '24.16.0\n\n',
  '24.16.0\n25.0.0',
]) assert.throws(() => parsePinnedNodeVersion(source), /canonical MAJOR\.MINOR\.PATCH/u)

assert.throws(
  () => parsePinnedNodeVersion('1'.repeat(65)),
  /bounded UTF-8 text/u,
)
assert.throws(
  () => parsePinnedNodeVersion(Buffer.from(PINNED_NODE_VERSION)),
  /bounded UTF-8 text/u,
)

const repositoryPin = readPinnedNodeVersion(REPOSITORY_ROOT)
assert.equal(repositoryPin, PINNED_NODE_VERSION)
assert.equal(
  assertPinnedNodeVersion({
    actualVersion: process.version,
    pinnedVersion: repositoryPin,
  }),
  PINNED_NODE_VERSION,
)
assert.equal(
  assertPinnedNodeVersion({
    actualVersion: `v${PINNED_NODE_VERSION}`,
    pinnedVersion: PINNED_NODE_VERSION,
  }),
  PINNED_NODE_VERSION,
)

assert.throws(
  () => assertPinnedNodeVersion({
    actualVersion: 'v24.16.1',
    pinnedVersion: PINNED_NODE_VERSION,
  }),
  /does not match repository pin/u,
)
assert.throws(
  () => assertPinnedNodeVersion({
    actualVersion: PINNED_NODE_VERSION,
    pinnedVersion: PINNED_NODE_VERSION,
  }),
  /active Node version must be canonical/u,
)
assert.throws(
  () => assertPinnedNodeVersion({
    actualVersion: `v${PINNED_NODE_VERSION}`,
    pinnedVersion: `v${PINNED_NODE_VERSION}`,
  }),
  /assertion value must be canonical/u,
)
assert.throws(
  () => readPinnedNodeVersion('.'),
  /repository root must be absolute and canonical/u,
)

process.stdout.write('repository Node version authority contracts: PASS\n')
