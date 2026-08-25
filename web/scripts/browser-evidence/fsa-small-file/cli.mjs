#!/usr/bin/env node
import { mkdir, readFile, writeFile } from 'node:fs/promises'
import { dirname, resolve } from 'node:path'
import { fileURLToPath } from 'node:url'
import { verifyHostTree } from './host-verification.mjs'
import { summarizePairedEvidence } from './results.mjs'
import { loadCanonicalWorkload, WORKLOAD_PATH } from './workload.mjs'

function parseArguments(argv) {
  const [command, ...rest] = argv
  const options = { baseline: [], product: [] }
  for (let index = 0; index < rest.length; index += 1) {
    const argument = rest[index]
    const value = rest[index + 1]
    if (argument === '--root' || argument === '--output') {
      if (value === undefined) throw new Error(`${argument} requires a value`)
      options[argument.slice(2)] = value
      index += 1
    } else if (argument === '--baseline' || argument === '--product') {
      if (value === undefined) throw new Error(`${argument} requires a value`)
      options[argument.slice(2)].push(value)
      index += 1
    } else {
      throw new Error(`Unknown argument: ${argument}`)
    }
  }
  return { command, options }
}

async function readJson(path) {
  return JSON.parse(await readFile(resolve(path), 'utf8'))
}

async function emit(value, output) {
  const body = `${JSON.stringify(value, null, 2)}\n`
  if (output === undefined) process.stdout.write(body)
  else {
    const path = resolve(output)
    await mkdir(dirname(path), { recursive: true })
    // Evidence is immutable by default; a repeated run must choose a fresh artifact path.
    await writeFile(path, body, { encoding: 'utf8', flag: 'wx' })
  }
}

export async function main(argv = process.argv.slice(2)) {
  const { command, options } = parseArguments(argv)
  const canonical = await loadCanonicalWorkload()
  if (command === 'validate-workload') {
    await emit({
      ok: true,
      workloadPath: WORKLOAD_PATH,
      workloadSha256: canonical.sha256,
      facts: canonical.workload.facts,
      digests: canonical.workload.digests,
    }, options.output)
    return
  }
  if (command === 'verify-host') {
    if (options.root === undefined) throw new Error('verify-host requires --root')
    await emit(await verifyHostTree({
      rootPath: options.root,
      workload: canonical.workload,
      workloadSha256: canonical.sha256,
    }), options.output)
    return
  }
  if (command === 'summarize') {
    if (options.baseline.length === 0 || options.product.length === 0) {
      throw new Error('summarize requires repeated --baseline and --product result paths')
    }
    const [baselineResults, productResults] = await Promise.all([
      Promise.all(options.baseline.map(readJson)),
      Promise.all(options.product.map(readJson)),
    ])
    for (const result of [...baselineResults, ...productResults]) {
      if (result.workloadSha256 !== canonical.sha256) throw new Error('Result does not reference the repository canonical workload')
    }
    await emit(summarizePairedEvidence({ baselineResults, productResults }), options.output)
    return
  }
  throw new Error('Usage: cli.mjs validate-workload | verify-host --root PATH | summarize --baseline FILE --product FILE [...] [--output FILE]')
}

if (process.argv[1] === fileURLToPath(import.meta.url)) {
  main().catch((error) => {
    process.stderr.write(`${error instanceof Error ? error.message : String(error)}\n`)
    process.exitCode = 1
  })
}
