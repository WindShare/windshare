import { join, resolve } from 'node:path'

import { describe, expect, it } from 'vitest'

import { directBinaryPaths } from '../../e2e/fixtures/direct-product-stack'

describe('direct product-stack executable paths', () => {
  it('derives Windows executables from the invocation-private directory', () => {
    const directory = resolve('test-results', 'invocation-a')

    expect(directBinaryPaths(directory, 'win32')).toEqual({
      directory,
      wind: join(directory, 'wind.exe'),
      relay: join(directory, 'wsrelay.exe'),
    })
  })

  it('keeps independent invocations disjoint', () => {
    const first = directBinaryPaths(resolve('test-results', 'invocation-a'), 'linux')
    const second = directBinaryPaths(resolve('test-results', 'invocation-b'), 'linux')

    expect(first.directory).not.toBe(second.directory)
    expect(first.wind).not.toBe(second.wind)
    expect(first.relay).not.toBe(second.relay)
    expect(first.wind).toBe(join(first.directory, 'wind'))
    expect(first.relay).toBe(join(first.directory, 'wsrelay'))
  })

  it('rejects a relative ownership root', () => {
    expect(() => directBinaryPaths('shared-bin', 'win32')).toThrow(
      'Direct product binary directory path must be absolute and canonical',
    )
  })
})
