import assert from 'node:assert/strict'
import { createHash } from 'node:crypto'
import { mkdtemp, mkdir, readFile, realpath, rm, writeFile } from 'node:fs/promises'
import { tmpdir } from 'node:os'
import { isAbsolute, join, relative, resolve } from 'node:path'

import {
  BOOTSTRAP_BUILD_RECEIPT_SCHEMA_VERSION,
  assertHeldBytesDigest,
  buildBootstrapProcessOwner,
  holdRegularFile,
  parseBootstrapGoModuleAuthority,
} from '../../process/bootstrap-build-authority.mjs'
import {
  BOOTSTRAP_GO_WORKSPACE_MODE,
  assertBootstrapGoSourceInventory,
  bootstrapOwnerPackage,
  createBootstrapGoSourceAuthority,
  parseBootstrapGoSourceInventory,
} from '../../process/bootstrap-go-source-authority.mjs'

const MAXIMUM_TEST_SOURCE_BYTES = 1_048_576
const TEST_GO_SUM = 'example.invalid/dependency v1.0.0 h1:AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA=\n'

verifyOwnerAuthority()
verifyInventoryContract()
verifyModuleMetadataContract()
verifyHeldBytesDigestContract()
await verifyMetadataPathContract()
await verifyPrivateBuildRecipe()
await verifyHeldSourceSnapshot()
await verifyMetadataIsolation()

process.stdout.write('bootstrap Go source authority contracts: PASS\n')

function verifyOwnerAuthority() {
  assert.equal(
    bootstrapOwnerPackage(
      'linux',
      './web/scripts/browser-evidence/linuxprocessowner',
    ).goOS,
    'linux',
  )
  assert.equal(
    bootstrapOwnerPackage(
      'win32',
      './web/scripts/browser-evidence/windowsjob',
    ).goOS,
    'windows',
  )
  assert.throws(
    () => bootstrapOwnerPackage('linux', './web/scripts/browser-evidence/windowsjob'),
    /restricted to the current platform owner package/u,
  )
}

function verifyInventoryContract() {
  const repositoryRoot = resolve(join(tmpdir(), 'windshare-bootstrap-contract-root'))
  const owner = bootstrapOwnerPackage(
    'linux',
    './web/scripts/browser-evidence/linuxprocessowner',
  )
  const encoded = inventoryJson(repositoryRoot, owner, [
    'supervision_linux.go',
    'main_linux.go',
  ])
  const inventory = parseBootstrapGoSourceInventory(encoded, { repositoryRoot, owner })
  assert.deepEqual(inventory.relativePaths, [
    'web/scripts/browser-evidence/linuxprocessowner/main_linux.go',
    'web/scripts/browser-evidence/linuxprocessowner/supervision_linux.go',
  ])

  const added = Object.freeze({
    ...inventory,
    relativePaths: Object.freeze([
      ...inventory.relativePaths,
      'web/scripts/browser-evidence/linuxprocessowner/termination_linux.go',
    ]),
  })
  const removed = Object.freeze({
    ...inventory,
    relativePaths: Object.freeze(inventory.relativePaths.slice(0, -1)),
  })
  assert.throws(
    () => assertBootstrapGoSourceInventory(inventory, added),
    /source inventory changed across the build/u,
  )
  assert.throws(
    () => assertBootstrapGoSourceInventory(inventory, removed),
    /source inventory changed across the build/u,
  )

  const unsupported = JSON.parse(encoded)
  unsupported.CgoFiles = ['native_linux.go']
  assert.throws(
    () => parseBootstrapGoSourceInventory(JSON.stringify(unsupported), { repositoryRoot, owner }),
    /unsupported compiled input field CgoFiles/u,
  )
  const embedded = JSON.parse(encoded)
  embedded.EmbedFiles = ['payload.bin']
  assert.throws(
    () => parseBootstrapGoSourceInventory(JSON.stringify(embedded), { repositoryRoot, owner }),
    /unsupported compiled input field EmbedFiles/u,
  )
  const repeated = JSON.parse(encoded)
  repeated.GoFiles.push(repeated.GoFiles[0])
  assert.throws(
    () => parseBootstrapGoSourceInventory(JSON.stringify(repeated), { repositoryRoot, owner }),
    /repeats a production source/u,
  )
}

function verifyModuleMetadataContract() {
  assert.deepEqual(
    parseBootstrapGoModuleAuthority(JSON.stringify({
      Module: { Path: 'github.com/windshare/windshare' },
      Replace: null,
    })),
    { modulePath: 'github.com/windshare/windshare' },
  )
  assert.throws(
    () => parseBootstrapGoModuleAuthority(JSON.stringify({
      Module: { Path: 'github.com/windshare/windshare' },
      Replace: [{
        Old: { Path: 'github.com/windshare/windshare/core' },
        New: { Path: '../core' },
      }],
    })),
    /cannot prove replace directives are absent/u,
  )
  assert.throws(
    () => parseBootstrapGoModuleAuthority(JSON.stringify({
      Module: { Path: 'github.com/windshare/windshare' },
    })),
    /cannot prove replace directives are absent/u,
  )
}

function verifyHeldBytesDigestContract() {
  const heldBytes = Buffer.from('held bytes', 'utf8')
  const expectedSha256 = createHash('sha256').update(heldBytes).digest('hex')
  assert.doesNotThrow(() => assertHeldBytesDigest(heldBytes, expectedSha256, 'test source'))
  heldBytes[0] ^= 0xff
  assert.throws(
    () => assertHeldBytesDigest(heldBytes, expectedSha256, 'test source'),
    /returned bytes outside its held digest/u,
  )
  heldBytes.fill(0)
}

async function verifyMetadataPathContract() {
  const owner = bootstrapOwnerPackage(
    'linux',
    './web/scripts/browser-evidence/linuxprocessowner',
  )
  const options = {
    repositoryRoot: resolve(join(tmpdir(), 'windshare-bootstrap-invalid-metadata')),
    runtimeRoot: resolve(join(tmpdir(), 'windshare-bootstrap-invalid-runtime')),
    owner,
    maximumSourceBytes: MAXIMUM_TEST_SOURCE_BYTES,
    holdFile: holdRegularFile,
  }
  await assert.rejects(
    createBootstrapGoSourceAuthority({
      ...options,
      metadataRelativePaths: ['go.mod', 'go.mod'],
    }),
    /module metadata paths are invalid/u,
  )
  await assert.rejects(
    createBootstrapGoSourceAuthority({
      ...options,
      metadataRelativePaths: ['..'],
    }),
    /module metadata paths are invalid/u,
  )
}

async function verifyPrivateBuildRecipe() {
  const temporaryRoot = await realpath(await mkdtemp(join(tmpdir(), 'windshare-bootstrap-recipe-')))
  const repositoryRoot = join(temporaryRoot, 'repository')
  const runtimeRoot = join(temporaryRoot, 'runtime')
  const owner = bootstrapOwnerPackage(
    'win32',
    './web/scripts/browser-evidence/windowsjob',
  )
  const packageRoot = join(repositoryRoot, ...owner.packagePath.slice(2).split('/'))
  const goExecutable = join(temporaryRoot, 'fake-go.exe')
  await Promise.all([
    mkdir(packageRoot, { recursive: true }),
    mkdir(runtimeRoot),
  ])
  await Promise.all([
    writeFile(join(repositoryRoot, 'go.mod'), 'module github.com/windshare/windshare\n\ngo 1.26.5\n'),
    writeFile(join(repositoryRoot, 'go.sum'), TEST_GO_SUM),
    writeFile(join(repositoryRoot, 'go.work'), 'go 1.26.5\n\nuse ./untrusted\n'),
    writeFile(join(packageRoot, 'main.go'), 'package main\n\nfunc main() {}\n'),
    writeFile(goExecutable, 'sealed test toolchain\n'),
  ])
  let authority
  try {
    const calls = []
    authority = await buildBootstrapProcessOwner({
      repositoryRoot,
      runtimeRoot,
      platform: 'win32',
      architecture: 'x64',
      goExecutable,
      outputPath: join(runtimeRoot, 'owner.exe'),
      packagePath: owner.packagePath,
      cwd: repositoryRoot,
      runProcess: fakeBuildRunner({ owner, calls, replaceDirective: false }),
    })
    assert.deepEqual(calls.map(({ operation }) => operation), [
      'bootstrap-go-version',
      'bootstrap-go-module-edit',
      'bootstrap-go-source-list',
      'bootstrap-go-source-list',
      'bootstrap-build-windows-job',
      'bootstrap-go-source-list',
    ])
    const snapshotRoot = calls[0].cwd
    assert.equal(pathIsInside(runtimeRoot, snapshotRoot), true)
    for (const call of calls) {
      assert.equal(call.cwd, snapshotRoot)
      assert.equal(call.environment.GOWORK, 'off')
      assert.equal(call.environment.GOENV, 'off')
      assert.equal(call.environment.GOFLAGS, '')
    }
    assert.deepEqual(calls[0].arguments, ['version'])
    assert.deepEqual(calls[1].arguments, ['mod', 'edit', '-json'])
    for (const listCall of [calls[2], calls[3], calls[5]]) {
      assert.deepEqual(listCall.arguments, ['list', '-mod=readonly', '-json', owner.packagePath])
      assert.equal(listCall.recipeIdentity, 'windshare.bootstrap-go-source-list/v2')
    }
    const buildCall = calls[4]
    const outputIndex = buildCall.arguments.indexOf('-o')
    const buildPaths = buildCall.arguments.slice(outputIndex + 2)
    assert.equal(outputIndex > 0, true)
    assert.deepEqual(buildPaths, [join(snapshotRoot, ...owner.packagePath.slice(2).split('/'), 'main.go')])
    assert.equal(buildPaths.every((path) => pathIsInside(snapshotRoot, path)), true)
    assert.equal(buildCall.recipeIdentity, 'windshare.bootstrap-owner-build/windows-job/v2')
    assert.equal(BOOTSTRAP_BUILD_RECEIPT_SCHEMA_VERSION, 'windshare.bootstrap-build-receipt/v2')
    assert.equal(authority.receipt.recipe.identity, buildCall.recipeIdentity)
    assert.equal(authority.receipt.recipe.cwd, snapshotRoot)
    assert.equal(authority.receipt.recipe.environment.GOWORK, 'off')
    assert.deepEqual(
      authority.receipt.source.files.map(({ relativePath }) => relativePath),
      ['go.mod', 'go.sum', `${owner.packagePath.slice(2)}/main.go`],
    )
    await authority.close()
    authority = undefined

    const rejectedCalls = []
    await assert.rejects(
      buildBootstrapProcessOwner({
        repositoryRoot,
        runtimeRoot,
        platform: 'win32',
        architecture: 'x64',
        goExecutable,
        outputPath: join(runtimeRoot, 'rejected-owner.exe'),
        packagePath: owner.packagePath,
        cwd: repositoryRoot,
        runProcess: fakeBuildRunner({ owner, calls: rejectedCalls, replaceDirective: true }),
      }),
      /cannot prove replace directives are absent/u,
    )
    assert.deepEqual(rejectedCalls.map(({ operation }) => operation), [
      'bootstrap-go-version',
      'bootstrap-go-module-edit',
    ])
    assert.equal(rejectedCalls.every(({ cwd }) => pathIsInside(runtimeRoot, cwd)), true)
    assert.equal(new Set(rejectedCalls.map(({ cwd }) => cwd)).size, 1)
    for (const call of rejectedCalls) {
      assert.equal(call.environment.GOWORK, 'off')
      assert.equal(call.environment.GOENV, 'off')
      assert.equal(call.environment.GOFLAGS, '')
    }
  } finally {
    await authority?.close()
    await rm(temporaryRoot, { recursive: true, force: true })
  }
}

function fakeBuildRunner({ owner, calls, replaceDirective }) {
  return async ({ arguments: arguments_, cwd, environment, operation, recipeIdentity }) => {
    calls.push({
      arguments: [...arguments_],
      cwd,
      environment: { ...environment },
      operation,
      recipeIdentity,
    })
    if (operation === 'bootstrap-go-version') {
      return { stdout: 'go version go1.26.5 windows/amd64\n', stderr: '' }
    }
    if (operation === 'bootstrap-go-module-edit') {
      return {
        stdout: JSON.stringify({
          Module: { Path: 'github.com/windshare/windshare' },
          Replace: replaceDirective
            ? [{ Old: { Path: 'example.invalid/dependency' }, New: { Path: '../untrusted' } }]
            : null,
        }),
        stderr: '',
      }
    }
    if (operation === 'bootstrap-go-source-list') {
      return { stdout: inventoryJson(cwd, owner, ['main.go']), stderr: '' }
    }
    if (operation === 'bootstrap-build-windows-job') {
      const outputIndex = arguments_.indexOf('-o')
      assert.equal(outputIndex > 0, true)
      await writeFile(arguments_[outputIndex + 1], 'sealed owner output\n')
      return { stdout: '', stderr: '' }
    }
    throw new Error(`unexpected fake bootstrap operation ${operation}`)
  }
}

async function verifyHeldSourceSnapshot() {
  const temporaryRoot = await realpath(await mkdtemp(join(tmpdir(), 'windshare-bootstrap-source-')))
  const repositoryRoot = join(temporaryRoot, 'repository')
  const runtimeRoot = join(temporaryRoot, 'runtime')
  const owner = bootstrapOwnerPackage(
    'linux',
    './web/scripts/browser-evidence/linuxprocessowner',
  )
  const packageRoot = join(repositoryRoot, ...owner.packagePath.slice(2).split('/'))
  await Promise.all([
    mkdir(packageRoot, { recursive: true }),
    mkdir(runtimeRoot),
  ])
  const sourceContent = Object.freeze({
    'main_linux.go': 'package main\n\nfunc main() {}\n',
    'supervision_linux.go': 'package main\n\nfunc supervise() {}\n',
  })
  for (const [filename, content] of Object.entries(sourceContent)) {
    await writeFile(join(packageRoot, filename), content)
  }
  await Promise.all([
    writeFile(join(repositoryRoot, 'go.mod'), 'module github.com/windshare/windshare\n\ngo 1.26.5\n'),
    writeFile(join(repositoryRoot, 'go.sum'), TEST_GO_SUM),
  ])
  let authority
  try {
    authority = await createBootstrapGoSourceAuthority({
      repositoryRoot,
      runtimeRoot,
      owner,
      metadataRelativePaths: ['go.mod', 'go.sum'],
      maximumSourceBytes: MAXIMUM_TEST_SOURCE_BYTES,
      holdFile: holdRegularFile,
    })
    const inventory = parseBootstrapGoSourceInventory(
      inventoryJson(authority.moduleRoot, owner, Object.keys(sourceContent)),
      { repositoryRoot: authority.moduleRoot, owner },
    )
    const selection = authority.select(inventory)
    assert.deepEqual(
      selection.receiptSources.map(({ relativePath }) => relativePath),
      ['go.mod', 'go.sum', ...inventory.relativePaths],
    )
    assert.equal(selection.buildPaths.length, inventory.relativePaths.length)
    for (const [index, buildPath] of selection.buildPaths.entries()) {
      const expected = sourceContent[inventory.relativePaths[index].split('/').at(-1)]
      assert.equal(await readFile(buildPath, 'utf8'), expected)
    }
    await authority.assertLive()

    const addedPath = join(packageRoot, 'termination_linux.go')
    await writeFile(addedPath, 'package main\n\nfunc terminate() {}\n')
    await assert.rejects(
      authority.assertLive(),
      /candidate inventory changed across the build/u,
    )
    await rm(addedPath)
    await authority.assertLive()

    const replacedPath = join(packageRoot, 'main_linux.go')
    await writeFile(replacedPath, 'package main\n\nfunc main() { panic("replaced") }\n')
    await assert.rejects(
      authority.assertLive(),
      /changed while held|digest changed while held/u,
    )
  } finally {
    await authority?.close()
    await rm(temporaryRoot, { recursive: true, force: true })
  }
}

async function verifyMetadataIsolation() {
  const temporaryRoot = await realpath(await mkdtemp(join(tmpdir(), 'windshare-bootstrap-metadata-')))
  const repositoryRoot = join(temporaryRoot, 'repository')
  const runtimeRoot = join(temporaryRoot, 'runtime')
  const owner = bootstrapOwnerPackage(
    'linux',
    './web/scripts/browser-evidence/linuxprocessowner',
  )
  const packageRoot = join(repositoryRoot, ...owner.packagePath.slice(2).split('/'))
  await Promise.all([
    mkdir(packageRoot, { recursive: true }),
    mkdir(runtimeRoot),
  ])
  const originalGoMod = 'module github.com/windshare/windshare\n\ngo 1.26.5\n'
  const goModPath = join(repositoryRoot, 'go.mod')
  const goWorkPath = join(repositoryRoot, 'go.work')
  await Promise.all([
    writeFile(goModPath, originalGoMod),
    writeFile(join(repositoryRoot, 'go.sum'), TEST_GO_SUM),
    writeFile(goWorkPath, 'go 1.26.5\n\nuse .\n'),
    writeFile(join(packageRoot, 'main_linux.go'), 'package main\n\nfunc main() {}\n'),
    writeFile(join(packageRoot, 'main_windows.go'), 'package main\n\nfunc windowsOnly() {}\n'),
  ])
  let authority
  try {
    authority = await createBootstrapGoSourceAuthority({
      repositoryRoot,
      runtimeRoot,
      owner,
      metadataRelativePaths: ['go.mod', 'go.sum'],
      maximumSourceBytes: MAXIMUM_TEST_SOURCE_BYTES,
      holdFile: holdRegularFile,
    })
    const inventory = parseBootstrapGoSourceInventory(
      inventoryJson(authority.moduleRoot, owner, ['main_linux.go']),
      { repositoryRoot: authority.moduleRoot, owner },
    )
    const selection = authority.select(inventory)
    assert.equal(BOOTSTRAP_GO_WORKSPACE_MODE, 'off')
    assert.deepEqual(
      selection.receiptSources.map(({ relativePath }) => relativePath),
      ['go.mod', 'go.sum', ...inventory.relativePaths],
    )
    const snapshotGoSum = await authority.readSnapshotMetadata('go.sum')
    try {
      assert.equal(snapshotGoSum.toString('utf8'), TEST_GO_SUM)
    } finally {
      snapshotGoSum.fill(0)
    }
    await assert.rejects(
      authority.readSnapshotMetadata('go.work'),
      /omitted requested metadata/u,
    )
    for (const buildPath of selection.buildPaths) {
      const snapshotRelativePath = relative(authority.moduleRoot, buildPath)
      assert.equal(snapshotRelativePath.startsWith('..') || isAbsolute(snapshotRelativePath), false)
    }
    await assert.rejects(readFile(join(authority.moduleRoot, 'go.work')), { code: 'ENOENT' })

    await writeFile(goWorkPath, 'go 1.26.5\n\nuse ./untrusted\n')
    await authority.assertLive()

    await writeFile(
      goModPath,
      'module example.invalid/replaced\n\ngo 1.26.5\n',
    )
    assert.equal(await readFile(join(authority.moduleRoot, 'go.mod'), 'utf8'), originalGoMod)
    assert.deepEqual(authority.select(inventory).buildPaths, selection.buildPaths)
    await assert.rejects(
      authority.assertLive(),
      /changed while held|digest changed while held/u,
    )

    await authority.close()
    await writeFile(goModPath, originalGoMod)
    authority = await createBootstrapGoSourceAuthority({
      repositoryRoot,
      runtimeRoot,
      owner,
      metadataRelativePaths: ['go.mod', 'go.sum'],
      maximumSourceBytes: MAXIMUM_TEST_SOURCE_BYTES,
      holdFile: holdRegularFile,
    })
    await writeFile(join(authority.moduleRoot, 'go.sum'), 'tampered snapshot\n')
    await assert.rejects(
      authority.assertLive(),
      /source snapshot go\.sum changed while held|source snapshot go\.sum digest changed while held/u,
    )
  } finally {
    await authority?.close()
    await rm(temporaryRoot, { recursive: true, force: true })
  }
}

function inventoryJson(repositoryRoot, owner, goFiles) {
  return JSON.stringify({
    Dir: join(repositoryRoot, ...owner.packagePath.slice(2).split('/')),
    ImportPath: owner.importPath,
    Name: 'main',
    Root: repositoryRoot,
    Module: {
      Path: 'github.com/windshare/windshare',
      Main: true,
      Dir: repositoryRoot,
      GoMod: join(repositoryRoot, 'go.mod'),
    },
    GoFiles: goFiles,
  })
}

function pathIsInside(root, candidate) {
  const relativePath = relative(root, candidate)
  return relativePath !== '' && !relativePath.startsWith('..') && !isAbsolute(relativePath)
}
