import { mkdtemp, rm, writeFile } from 'node:fs/promises'
import { tmpdir } from 'node:os'
import { join } from 'node:path'

import { afterEach, describe, expect, it } from 'vitest'

import {
  PION_SERVER_EXECUTABLE_ENV,
  pionServerCommand,
} from '../../scripts/browser-evidence/pion-server-command.mjs'

const roots: string[] = []

afterEach(async () => {
  await Promise.all(roots.splice(0).map((root) => rm(root, { recursive: true, force: true })))
})

describe('Pion server command capability', () => {
  it('accepts only an absolute live regular-file capability', async () => {
    const root = await temporaryRoot()
    const executable = join(root, 'pion-server')
    await writeFile(executable, 'prebuilt fixture')

    const command = pionServerCommand({ [PION_SERVER_EXECUTABLE_ENV]: executable })
    expect(command).toEqual({ executable, arguments: [] })
    expect(Object.isFrozen(command)).toBe(true)
    expect(Object.isFrozen(command.arguments)).toBe(true)
  })

  it('rejects missing, relative, empty, and directory capabilities', async () => {
    const root = await temporaryRoot()
    const empty = join(root, 'empty')
    await writeFile(empty, '')

    expect(() => pionServerCommand({})).toThrow(/absolute canonical path/u)
    expect(() => pionServerCommand({ [PION_SERVER_EXECUTABLE_ENV]: 'relative-server' }))
      .toThrow(/absolute canonical path/u)
    expect(() => pionServerCommand({ [PION_SERVER_EXECUTABLE_ENV]: empty }))
      .toThrow(/regular non-symbolic file/u)
    expect(() => pionServerCommand({ [PION_SERVER_EXECUTABLE_ENV]: root }))
      .toThrow(/regular non-symbolic file/u)
  })
})

async function temporaryRoot(): Promise<string> {
  const root = await mkdtemp(join(tmpdir(), 'windshare-pion-command-'))
  roots.push(root)
  return root
}
