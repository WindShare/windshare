import assert from 'node:assert/strict'
import { spawn, spawnSync } from 'node:child_process'
import { mkdir, mkdtemp, readFile, rm, writeFile } from 'node:fs/promises'
import { tmpdir } from 'node:os'
import { join } from 'node:path'
import { pathToFileURL } from 'node:url'
import test from 'node:test'
import { checkFiles } from './check.mjs'
import { goEnvironment } from './environment.mjs'

const tool = spawnSync('go', ['env', '-json', 'GOHOSTOS', 'GOHOSTARCH'], {
  encoding: 'utf8', windowsHide: true, env: goEnvironment(),
})
if (tool.error) throw tool.error
assert.equal(tool.status, 0, tool.stderr)
const host = JSON.parse(tool.stdout)
const native = { goos: host.GOHOSTOS, goarch: host.GOHOSTARCH, cgo_enabled: false }
const foreign = { goos: native.goos === 'linux' ? 'windows' : 'linux', goarch: 'amd64', cgo_enabled: false }

test('real gopls diagnoses fixed foreign profiles, siblings, and persisted-flag isolation', async (t) => {
  const root = await mkdtemp(join(tmpdir(), 'windshare-gopls-coverage-'))
  t.after(() => rm(root, { recursive: true, force: true }))
  const sources = {
    'go.mod': 'module example.test/diagnosticcoverage\n\ngo 1.26\n',
    'profile/common.go': 'package profile\n\nvar Number int = value\n',
    'profile/default.go': '//go:build !probe\n\npackage profile\n\nconst value = 42\n',
    'profile/tagged.go': '//go:build probe\n\npackage profile\n\nconst value = "wrong type"\n',
    [`foreign/tagged_${foreign.goos}.go`]: `//go:build ${foreign.goos} && probe\n\npackage foreign\n\nvar Number int = "wrong type"\n`,
    'foreign/common.go': 'package foreign\n\nfunc nativeOnly() {}\n',
    [`foreign/native_${native.goos}.go`]: 'package foreign\n\nvar _ = nativeOnly\n',
    'ambient/default.go': '//go:build !probe\n\npackage ambient\n\nvar Number int = "wrong type"\n',
    'ambient/tagged.go': '//go:build probe\n\npackage ambient\n\nvar Number int = 42\n',
  }
  for (const [file, content] of Object.entries(sources)) {
    const path = join(root, file)
    await mkdir(join(path, '..'), { recursive: true })
    await writeFile(path, content)
  }
  const goenv = join(root, 'go-env')
  const persisted = 'GOFLAGS=-tags=probe\n'
  await writeFile(goenv, persisted)
  const cases = [
    { name: 'shared file under tagged implementation', build: { ...native, tags: ['probe'] },
      files: ['profile/tagged.go'], diagnosticFiles: ['profile/tagged.go', 'profile/common.go'], error: 'profile/common.go' },
    { name: 'foreign platform and tag', build: { ...foreign, tags: ['probe'] },
      files: [`foreign/tagged_${foreign.goos}.go`],
      diagnosticFiles: [`foreign/tagged_${foreign.goos}.go`, 'foreign/common.go'], error: `foreign/tagged_${foreign.goos}.go` },
    { name: 'default profile overrides persisted GOFLAGS', build: native,
      files: ['ambient/default.go'], error: 'ambient/default.go' },
    { name: 'valid default implementation stays clean', build: native,
      files: ['profile/default.go'], diagnosticFiles: ['profile/default.go', 'profile/common.go'] },
  ]
  for (const scenario of cases) {
    await t.test(scenario.name, async () => {
      const diagnostics = await checkFiles({
        root, ...scenario, timeoutMs: 60_000,
        launch(command, args, options) {
          return spawn(command, args, { ...options, env: { ...options.env, GOENV: goenv } })
        },
      })
      if (scenario.error) {
        const uri = pathToFileURL(join(root, scenario.error)).href
        assert.ok(diagnostics.some((item) => item.uri === uri && item.severity === 1 &&
          item.message.includes('cannot use')), JSON.stringify(diagnostics))
        assert.ok(diagnostics.every((item) => item.severity === 1), JSON.stringify(diagnostics))
      } else assert.deepEqual(diagnostics, [])
    })
  }
  assert.equal(await readFile(goenv, 'utf8'), persisted)
})
