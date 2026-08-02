import { join, resolve } from 'node:path'

import { describe, expect, it } from 'vitest'

import { realStackBinaryPaths } from '../../e2e/fixtures/v2-real-stack'

describe('real-stack executable paths', () => {
  it('derives Windows executables from the invocation-private directory', () => {
    const directory = resolve('test-results', 'invocation-a')

    expect(realStackBinaryPaths(directory, 'win32')).toEqual({
      directory,
      windshare: join(directory, 'windshare-e2e.exe'),
      relay: join(directory, 'wsrelay.exe'),
      processOwner: join(directory, 'testprocessowner.exe'),
    })
  })

  it('keeps independent invocations disjoint', () => {
    const first = realStackBinaryPaths(resolve('test-results', 'invocation-a'), 'linux')
    const second = realStackBinaryPaths(resolve('test-results', 'invocation-b'), 'linux')

    expect(first.directory).not.toBe(second.directory)
    expect(first.windshare).not.toBe(second.windshare)
    expect(first.relay).not.toBe(second.relay)
    expect(first.windshare).toBe(join(first.directory, 'windshare-e2e'))
    expect(first.relay).toBe(join(first.directory, 'wsrelay'))
  })

  it('rejects a relative ownership root', () => {
    expect(() => realStackBinaryPaths('shared-bin', 'win32')).toThrow(
      'Real-stack binary directory must be absolute',
    )
  })
})
