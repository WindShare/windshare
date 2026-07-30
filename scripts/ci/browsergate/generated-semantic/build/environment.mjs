import { isAbsolute, resolve } from 'node:path'

const WINDOWS_DIRECTORY_NAMES = Object.freeze(['SYSTEMROOT', 'WINDIR'])

export function createGeneratedSemanticEnvironment({
  platform,
  temporaryRoot,
  inheritedEnvironment = {},
}) {
  requirePlatform(platform)
  requireAbsolutePath(temporaryRoot, 'generated semantic temporary root')
  if (!isRecord(inheritedEnvironment)) {
    throw new TypeError('generated semantic inherited environment must be an object')
  }

  // Starting from an empty object is the isolation boundary. The inherited map
  // is consulted only for Windows loader directories that Node itself requires.
  const environment = {
    NODE_ENV: 'production',
    TZ: 'UTC',
    LANG: 'C',
    LC_ALL: 'C',
    TMPDIR: temporaryRoot,
    TMP: temporaryRoot,
    TEMP: temporaryRoot,
  }
  if (platform === 'win32') {
    const systemRoot = windowsEnvironmentEntry(inheritedEnvironment, WINDOWS_DIRECTORY_NAMES)
    if (systemRoot === undefined) {
      throw new Error('Windows generated semantic worker requires SystemRoot or WINDIR')
    }
    environment.SystemRoot = systemRoot
    environment.WINDIR = systemRoot
  }
  return Object.freeze(environment)
}

function windowsEnvironmentEntry(environment, names) {
  const values = []
  for (const name of Object.getOwnPropertyNames(environment)) {
    if (!names.includes(name.toUpperCase())) continue
    const descriptor = Object.getOwnPropertyDescriptor(environment, name)
    if (descriptor === undefined || !Object.hasOwn(descriptor, 'value')) {
      throw new Error(`Windows environment entry ${JSON.stringify(name)} must be a data property`)
    }
    const { value } = descriptor
    if (typeof value !== 'string' || value.length === 0 || value.includes('\0')) {
      throw new Error(`Windows environment entry ${JSON.stringify(name)} is invalid`)
    }
    values.push(value)
  }
  if (new Set(values).size > 1) {
    throw new Error('Windows generated semantic worker has conflicting SystemRoot entries')
  }
  return values[0]
}

function requirePlatform(value) {
  if (!['darwin', 'linux', 'win32'].includes(value)) {
    throw new Error('generated semantic worker platform is unsupported')
  }
}

function requireAbsolutePath(value, label) {
  if (typeof value !== 'string' || !isAbsolute(value) || resolve(value) !== value) {
    throw new Error(`${label} must be absolute and canonical`)
  }
}

function isRecord(value) {
  return typeof value === 'object' && value !== null && !Array.isArray(value)
}
