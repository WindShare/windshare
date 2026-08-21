import { describe, expect, it } from 'vitest'
import {
  RestorationTemplateUnavailableError,
  WINDOWS_POWERSHELL_V1_RESTORATION_TEMPLATE_ID,
  orderRestorationMappings,
  provenRestorationTemplateProvider,
  rebaseRestorationAncestors,
  restorationPathState,
} from '../../src/output/file-system-access/compatible-name/restoration-template'
import type {
  CompatibleNameSidecarMappingV1,
} from '../../src/output/file-system-access/compatible-name/sidecar-codec'

describe('compatible-name restoration policy', () => {
  it.each([
    { source: true, target: false, state: 'rename' },
    { source: false, target: true, state: 'already-restored' },
    { source: true, target: true, state: 'conflict' },
    { source: false, target: false, state: 'missing' },
  ] as const)('models source=$source target=$target as $state', ({ source, target, state }) => {
    expect(restorationPathState(source, target)).toBe(state)
  })

  it('orders deepest-first, descending ordinal within a depth, and the mapped root last', () => {
    const root = mapping(1, 'directory', ['result'], 'result.windshare-abc234')
    const directory = mapping(2, 'directory', ['result', 'nested'], 'nested.windshare-abc234')
    const firstLeaf = mapping(3, 'file', ['result', 'nested', 'first'], 'first.windshare-abc234')
    const laterLeaf = mapping(4, 'file', ['result', 'nested', 'later'], 'later.windshare-abc234')

    expect(orderRestorationMappings([root, laterLeaf, directory, firstLeaf]))
      .toEqual([laterLeaf, firstLeaf, directory, root])
  })

  it('rebases mapped ancestors across interruption without retaining stale physical parents', () => {
    const root = mapping(1, 'directory', ['result'], 'result.windshare-abc234')
    const directory = mapping(2, 'directory', ['result', 'nested'], 'nested.windshare-abc234')
    const leaf = mapping(3, 'file', ['result', 'nested', 'file.txt'], 'file.windshare-abc234')

    const beforeParentRestoration = rebaseRestorationAncestors(
      leaf,
      [root, directory, leaf],
      path => presence(path, {
        result: [false, true],
        'result/nested': [false, true],
      }),
    )
    expect(beforeParentRestoration).toMatchObject({
      state: 'resolved',
      parentComponents: ['result.windshare-abc234', 'nested.windshare-abc234'],
    })

    const afterRootRestoration = rebaseRestorationAncestors(
      leaf,
      [root, directory, leaf],
      path => presence(path, {
        result: [true, false],
        'result/nested': [false, true],
      }),
    )
    expect(afterRootRestoration).toMatchObject({
      state: 'resolved',
      parentComponents: ['result', 'nested.windshare-abc234'],
      steps: [
        { selectedName: 'logical' },
        { selectedName: 'physical' },
      ],
    })
  })

  it.each([
    { logicalPresent: true, physicalPresent: true, state: 'conflict' },
    { logicalPresent: false, physicalPresent: false, state: 'missing' },
  ] as const)('stops ancestor rebasing on $state', (observed) => {
    const root = mapping(1, 'directory', ['result'], 'result.windshare-abc234')
    const leaf = mapping(2, 'file', ['result', 'file.txt'], 'file.windshare-abc234')
    expect(rebaseRestorationAncestors(leaf, [root, leaf], () => observed))
      .toMatchObject({ state: observed.state, logicalPath: ['result'] })
  })

  it('exposes only the matching-host proven Windows template', () => {
    const template = provenRestorationTemplateProvider.select('windows')
    expect(template.id).toBe(WINDOWS_POWERSHELL_V1_RESTORATION_TEMPLATE_ID)
    expect(template.scriptFileExtension).toBe('.ps1')
    expect(template.source).toContain(
      "$script:WindShareRestorationTemplateId = 'windows-powershell-v1'",
    )
    for (const unsupported of ['linux', 'macos', 'unknown', 'Windows']) {
      expect(() => provenRestorationTemplateProvider.select(unsupported))
        .toThrow(RestorationTemplateUnavailableError)
    }
  })
})

function mapping(
  ordinal: number,
  entryKind: CompatibleNameSidecarMappingV1['entryKind'],
  logicalPath: readonly string[],
  physicalComponent: string,
): CompatibleNameSidecarMappingV1 {
  return Object.freeze({ ordinal, entryKind, logicalPath: Object.freeze([...logicalPath]), physicalComponent })
}

function presence(
  path: readonly string[],
  states: Readonly<Record<string, readonly [logical: boolean, physical: boolean]>>,
): Readonly<{ logicalPresent: boolean; physicalPresent: boolean }> {
  const value = states[path.join('/')]
  if (value === undefined) throw new Error(`missing test observation for ${path.join('/')}`)
  return Object.freeze({ logicalPresent: value[0], physicalPresent: value[1] })
}
