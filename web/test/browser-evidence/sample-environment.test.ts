import { describe, expect, it } from 'vitest'

import {
  inheritedSampleEnvironment,
  sampleProcessEnvironment,
} from '../../scripts/browser-evidence/process/sample-environment.ts'

describe('browser sample process environment', () => {
  it('inherits only runtime prerequisites and excludes ambient credentials', () => {
    // Pin win32 canonicalization so the mixed-case spellings above are
    // exercised identically on every host instead of passing by accident.
    expect(inheritedSampleEnvironment({
      Path: '/runtime/bin',
      home: '/runtime/home',
      GITHUB_TOKEN: 'github-secret',
      REPOSITORY_DEPLOY_SECRET: 'repository-secret',
      NODE_OPTIONS: '--require hostile.js',
    }, 'win32')).toEqual({
      PATH: '/runtime/bin',
      HOME: '/runtime/home',
    })
  })

  it('gives equivalent Windows environment spellings one canonical identity', () => {
    expect(inheritedSampleEnvironment({
      Path: 'C:\\runtime\\bin',
      ComSpec: 'C:\\Windows\\System32\\cmd.exe',
    }, 'win32')).toEqual(inheritedSampleEnvironment({
      PATH: 'C:\\runtime\\bin',
      COMSPEC: 'C:\\Windows\\System32\\cmd.exe',
    }, 'win32'))
    expect(inheritedSampleEnvironment({ Path: '/not-a-linux-path' }, 'linux')).toEqual({})
  })

  it('merges explicit synthetic inputs while rejecting known credential channels', () => {
    expect(sampleProcessEnvironment(
      { SYNTHETIC_CHILD_MODE: 'main-pass' },
      { WINDSHARE_BROWSER_EVIDENCE_CONTEXT: '{}' },
      { PATH: '/runtime/bin', GITHUB_TOKEN: 'ambient-secret' },
    )).toEqual({
      PATH: '/runtime/bin',
      SYNTHETIC_CHILD_MODE: 'main-pass',
      WINDSHARE_BROWSER_EVIDENCE_CONTEXT: '{}',
    })
    expect(() => sampleProcessEnvironment(
      { GITHUB_TOKEN: 'explicit-secret' },
      {},
      {},
    )).toThrow(/forbidden credential channel/u)
  })
})
