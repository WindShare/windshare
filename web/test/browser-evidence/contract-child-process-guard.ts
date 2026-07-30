import { afterAll, expect, vi } from 'vitest'

type ChildProcessCommand =
  | 'exec'
  | 'execFile'
  | 'execFileSync'
  | 'execSync'
  | 'fork'
  | 'spawn'
  | 'spawnSync'
const invocations: string[] = []

const hostile = (command: ChildProcessCommand) => () => {
  invocations.push(command)
  throw new Error(`browser-contract attempted forbidden child_process.${command}`)
}

vi.mock('node:child_process', async (importOriginal) => {
  const actual = await importOriginal<typeof import('node:child_process')>()
  return {
    ...actual,
    exec: hostile('exec'),
    execFile: hostile('execFile'),
    execFileSync: hostile('execFileSync'),
    execSync: hostile('execSync'),
    fork: hostile('fork'),
    spawn: hostile('spawn'),
    spawnSync: hostile('spawnSync'),
  }
})

afterAll(() => {
  expect(invocations, 'browser-contract must execute zero child processes').toEqual([])
})
