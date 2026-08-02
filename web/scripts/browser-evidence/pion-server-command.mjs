import { lstatSync } from 'node:fs'
import { isAbsolute, resolve } from 'node:path'

export const PION_SERVER_EXECUTABLE_ENV = 'WINDSHARE_PION_SERVER_EXECUTABLE'

export function pionServerCommand(environment = process.env) {
  const path = environment[PION_SERVER_EXECUTABLE_ENV]
  if (typeof path !== 'string' || !isAbsolute(path) || resolve(path) !== path) {
    throw new Error(`${PION_SERVER_EXECUTABLE_ENV} must be an absolute canonical path`)
  }
  const metadata = lstatSync(path)
  if (!metadata.isFile() || metadata.isSymbolicLink() || metadata.size < 1) {
    throw new Error('Pion server executable must be a regular non-symbolic file')
  }
  return Object.freeze({ executable: path, arguments: Object.freeze([]) })
}
