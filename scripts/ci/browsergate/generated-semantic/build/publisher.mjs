import { randomBytes } from 'node:crypto'
import { open, rename, unlink } from 'node:fs/promises'
import { basename, dirname, isAbsolute, join, resolve } from 'node:path'

import {
  requireValidatedGeneratedSemanticArtifact,
  validatedGeneratedSemanticBytes,
} from './artifact-policy.mjs'
import {
  createGeneratedSemanticFailure,
  generatedSemanticCauseMessage,
  isGeneratedSemanticFailure,
} from './failure.mjs'

const defaultFilesystem = Object.freeze({ open, rename, unlink })
const PUBLICATION_RESULT_KEYS = Object.freeze([
  'ok',
  'published',
  'temporaryPath',
  'failures',
])

export async function publishGeneratedSemanticArtifact({
  artifact,
  destination,
  filesystem = defaultFilesystem,
  temporaryToken = () => randomBytes(12).toString('hex'),
}) {
  const validated = requireValidatedGeneratedSemanticArtifact(artifact)
  requireDestination(destination, validated.fileName)
  requireFilesystem(filesystem)
  const token = temporaryToken()
  if (typeof token !== 'string' || !/^[A-Za-z0-9_-]{8,128}$/u.test(token)) {
    throw new TypeError('generated semantic publication token is invalid')
  }
  const temporaryPath = join(dirname(destination), `.${validated.fileName}.${token}.tmp`)
  const failures = []
  let handle = null
  let temporaryExists = false

  try {
    try {
      handle = await filesystem.open(temporaryPath, 'wx', 0o600)
      temporaryExists = true
    } catch (cause) {
      failures.push(publicationFailure('staging-open-failed', cause))
    }

    if (handle !== null) {
      try {
        await handle.writeFile(validatedGeneratedSemanticBytes(validated))
        await handle.sync()
      } catch (cause) {
        failures.push(publicationFailure('staging-write-failed', cause))
      }
      try {
        await handle.close()
      } catch (cause) {
        failures.push(createGeneratedSemanticFailure(
          failures.length === 0 ? 'publication' : 'cleanup',
          'staging-close-failed',
          generatedSemanticCauseMessage(cause, 'generated semantic staging file failed to close'),
        ))
      }
    }

    if (failures.length === 0) {
      try {
        await filesystem.rename(temporaryPath, destination)
        temporaryExists = false
      } catch (cause) {
        failures.push(publicationFailure('atomic-replace-failed', cause))
      }
    }
  } finally {
    if (temporaryExists) {
      try {
        await filesystem.unlink(temporaryPath)
      } catch (cause) {
        failures.push(createGeneratedSemanticFailure(
          'cleanup',
          'staging-unlink-failed',
          generatedSemanticCauseMessage(cause, 'generated semantic staging file cleanup failed'),
        ))
      }
    }
  }

  return Object.freeze({
    ok: failures.length === 0,
    published: failures.length === 0,
    temporaryPath,
    failures: Object.freeze(failures),
  })
}

export function requireGeneratedSemanticPublicationResult(value) {
  if (value === null || typeof value !== 'object' || Array.isArray(value)) {
    throw new TypeError('generated semantic publisher result must be an object')
  }
  const keys = Object.keys(value)
  if (
    keys.length !== PUBLICATION_RESULT_KEYS.length ||
    PUBLICATION_RESULT_KEYS.some((key) => !Object.hasOwn(value, key))
  ) throw new TypeError('generated semantic publisher result does not have exact keys')
  if (typeof value.ok !== 'boolean' || value.published !== value.ok) {
    throw new TypeError('generated semantic publisher result flags contradict each other')
  }
  if (
    typeof value.temporaryPath !== 'string' || !isAbsolute(value.temporaryPath) ||
    resolve(value.temporaryPath) !== value.temporaryPath
  ) throw new TypeError('generated semantic publisher temporary path is invalid')
  if (
    !Array.isArray(value.failures) ||
    value.failures.some((failure) =>
      !isGeneratedSemanticFailure(failure) ||
      !['publication', 'cleanup'].includes(failure.kind)) ||
    (value.ok ? value.failures.length !== 0 : value.failures.length === 0)
  ) throw new TypeError('generated semantic publisher failures contradict its outcome')
  return Object.freeze({
    ok: value.ok,
    published: value.published,
    temporaryPath: value.temporaryPath,
    failures: Object.freeze(value.failures.map((failure) => Object.freeze({ ...failure }))),
  })
}

function publicationFailure(code, cause) {
  return createGeneratedSemanticFailure(
    'publication',
    code,
    generatedSemanticCauseMessage(cause, 'generated semantic publication failed'),
  )
}

function requireDestination(path, expectedName) {
  if (typeof path !== 'string' || !isAbsolute(path) || resolve(path) !== path) {
    throw new TypeError('generated semantic publication destination must be absolute and canonical')
  }
  if (basename(path) !== expectedName) {
    throw new TypeError('generated semantic publication destination has an unexpected file name')
  }
}

function requireFilesystem(filesystem) {
  if (
    filesystem === null || typeof filesystem !== 'object' ||
    ['open', 'rename', 'unlink'].some((name) => typeof filesystem[name] !== 'function')
  ) throw new TypeError('generated semantic publication filesystem is invalid')
}
