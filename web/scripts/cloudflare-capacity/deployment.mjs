import { execFile } from 'node:child_process'
import { createHash, randomBytes, randomUUID } from 'node:crypto'
import { mkdir, readFile, rm, writeFile } from 'node:fs/promises'
import { createRequire } from 'node:module'
import { dirname, join, resolve } from 'node:path'
import { fileURLToPath } from 'node:url'
import { promisify } from 'node:util'

export const REPOSITORY = resolve(dirname(fileURLToPath(import.meta.url)), '../../..')
export const WEB = join(REPOSITORY, 'web')
const PACKAGE = join(REPOSITORY, 'relay-rust/crates/relay-cloudflare')
const WRANGLER = join(PACKAGE, 'node_modules/wrangler/bin/wrangler.js')
const COMMAND_TIMEOUT_MS = 180_000
const MAX_OUTPUT_BYTES = 4 * 1024 * 1024
const require = createRequire(join(PACKAGE, 'package.json'))
const { unstable_readConfig } = require('wrangler')

export const digest = bytes => createHash('sha256').update(bytes).digest('hex')
export const save = (path, value) => writeFile(path, JSON.stringify(value, null, 2) + '\n')
export const event = (name, detail = {}) => console.info(JSON.stringify({ event: name, at: new Date().toISOString(), ...detail }))

export async function command(executable, args, cwd, environment = {}) {
  return promisify(execFile)(executable, args, {
    cwd, env: { ...process.env, ...environment }, windowsHide: true,
    timeout: COMMAND_TIMEOUT_MS, maxBuffer: MAX_OUTPUT_BYTES,
  })
}

export async function build(output) {
  await mkdir(output, { recursive: true })
  const executable = join(output, process.platform === 'win32' ? 'wind.exe' : 'wind')
  const frontend = join(output, 'frontend')
  for (const [stage, executableName, args, cwd] of [
    ['rust-build', 'worker-build', ['--release'], PACKAGE],
    ['go-build', 'go', ['build', '-o', executable, './cmd/wind'], REPOSITORY],
    ['frontend-build', process.execPath, [join(WEB, 'node_modules/vite/bin/vite.js'), 'build', '--outDir', frontend], WEB],
  ]) {
    event(stage)
    const result = await command(executableName, args, cwd, { GOWORK: 'off', GOTOOLCHAIN: 'local' })
    await writeFile(join(output, stage + '.log'), result.stdout + result.stderr)
  }
  const identity = {
    revision: (await command('git', ['rev-parse', 'HEAD'], REPOSITORY)).stdout.trim(),
    workingTree: (await command('git', ['status', '--short'], REPOSITORY)).stdout.trim(),
    node: process.version,
    wrangler: JSON.parse(await readFile(join(PACKAGE, 'node_modules/wrangler/package.json'), 'utf8')).version,
    playwright: JSON.parse(await readFile(join(WEB, 'node_modules/@playwright/test/package.json'), 'utf8')).version,
    wasmSha256: digest(await readFile(join(PACKAGE, 'build/index_bg.wasm'))),
    entrySha256: digest(await readFile(join(PACKAGE, 'build/index.js'))),
    cliSha256: digest(await readFile(executable)),
    configurationSha256: digest(await readFile(join(PACKAGE, 'wrangler.toml'))),
    proxyEnvironmentPresent: Object.keys(process.env).filter(key => /^(https?|all)_proxy$/i.test(key)),
  }
  await save(join(output, 'build.json'), identity)
  return { executable, frontend, identity }
}

/** A production entry and a fresh namespace keep other scenarios out of its metrics. */
export class ProductionDeployment {
  workerName = 'windshare-capacity-' + randomUUID().replaceAll('-', '').slice(0, 12)
  accessKey = ''
  #ownerPassword = randomBytes(32).toString('base64url')
  #output
  #config
  #secrets
  #attempted = false
  #consoleAttempted = false
  #consoleConfig
  constructor(output) {
    this.#output = output
    this.#config = join(output, 'wrangler.json')
    this.#consoleConfig = join(output, 'console.wrangler.json')
    this.#secrets = join(output, 'secrets.json')
    this.relayConfig = join(output, 'relays.json')
  }

  async provision() {
    await mkdir(this.#output, { recursive: true })
    const host = unstable_readConfig({ config: join(PACKAGE, 'wrangler.toml') })
    if (host.durable_objects.bindings.some(binding => binding.script_name)) {
      throw new Error('Capacity evidence cannot share another Worker namespace')
    }
    const configuration = {
      name: this.workerName, main: join(PACKAGE, 'entry.mjs'),
      compatibility_date: host.compatibility_date, compatibility_flags: host.compatibility_flags,
      migrations: host.migrations, durable_objects: host.durable_objects,
      observability: host.observability, workers_dev: true, preview_urls: false,
    }
    await save(this.#config, configuration)
    await writeFile(this.#secrets, JSON.stringify({ OWNER_PASSWORD: this.#ownerPassword }), { mode: 0o600 })
    this.#attempted = true
    try {
      await this.#wrangler('deploy', ['deploy', '--secrets-file', this.#secrets])
    } finally {
      await rm(this.#secrets, { force: true })
    }
    const records = (await readFile(join(this.#output, 'deploy.jsonl'), 'utf8')).trim().split('\n').map(JSON.parse)
    const record = records.find(item => item.type === 'deploy')
    if (record?.worker_name !== this.workerName || !record.version_id) throw new Error('Missing deployment identity')
    const url = record.targets.find(target => target.startsWith('https://'))
    if (!url || new URL(url).hostname.split('.')[0] !== this.workerName) throw new Error('Unexpected deployment URL')
    this.url = new URL(url).origin
    const consoleUrl = this.url.replace(this.workerName + '.', this.workerName + '-console.')
    await save(this.#config, { ...configuration, vars: { RELAY_URL: this.url, CONSOLE_ORIGIN: consoleUrl } })
    await this.#wrangler('access-config', ['deploy', '--keep-vars'])
    await save(this.#consoleConfig, {
      name: this.workerName + '-console', main: join(PACKAGE, 'console-entry.mjs'),
      compatibility_date: host.compatibility_date, workers_dev: true, preview_urls: false,
      vars: { CONSOLE_ORIGIN: consoleUrl },
      durable_objects: { bindings: [{ name: 'ACCESS', class_name: 'AccessObject', script_name: this.workerName }] },
    })
    this.#consoleAttempted = true
    await this.#wrangler('console', ['deploy'], this.#consoleConfig)
    const post = (path, body, cookie = '') => fetch(consoleUrl + '/api/' + path, {
      method: 'POST', headers: { 'Content-Type': 'application/json', Origin: consoleUrl, Cookie: cookie },
      body: JSON.stringify(body), redirect: 'error', signal: AbortSignal.timeout(COMMAND_TIMEOUT_MS),
    })
    const login = await post('login', { password: this.#ownerPassword })
    if (login.status !== 200) throw new Error('Console login failed: ' + login.status)
    const created = await post('credentials', { name: 'Capacity evidence' }, login.headers.get('Set-Cookie').split(';')[0])
    if (created.status !== 201) throw new Error('Credential creation failed: ' + created.status)
    this.accessKey = (await created.json()).access_key
    const finalRecord = (await readFile(join(this.#output, 'access-config.jsonl'), 'utf8'))
      .trim().split('\n').map(JSON.parse).find(item => item.type === 'deploy')
    if (!finalRecord || finalRecord.worker_name !== this.workerName) throw new Error('Missing configured deployment')
    record.version_id = finalRecord.version_id
    const version = JSON.parse(await this.#wrangler('version', ['versions', 'view', record.version_id, '--json']))
    this.identity = { workerName: this.workerName, url: this.url, versionId: record.version_id, version }
    await save(join(this.#output, 'deployment.json'), this.identity)
    await writeFile(this.relayConfig, JSON.stringify({
      profiles: [{ url: this.url, access_key: this.accessKey }], active_relays: [this.url],
    }), { mode: 0o600 })
    event('capacity-deployed', { workerName: this.workerName, url: this.url, versionId: record.version_id })
    return this
  }

  async dispose() {
    let deleted = false
    try {
      const errors = []
      if (this.#consoleAttempted) {
        try { await this.#wrangler('console-cleanup', ['delete', this.workerName + '-console'], this.#consoleConfig) }
        catch (error) { errors.push(error) }
      }
      if (this.#attempted) {
        try { await this.#wrangler('cleanup', ['delete', this.workerName]); deleted = true }
        catch (error) { errors.push(error) }
      }
      if (errors.length) throw new AggregateError(errors, 'Owned capacity Worker cleanup failed')
    } finally {
      await rm(this.#secrets, { force: true })
      await rm(this.relayConfig, { force: true })
      await save(join(this.#output, 'cleanup.json'), { workerName: this.workerName, deleted, at: new Date().toISOString() })
    }
  }

  async #wrangler(stage, args, config = this.#config) {
    const result = await command(process.execPath, [WRANGLER, ...args, '--config', config], PACKAGE, {
      CI: 'true', WRANGLER_SEND_METRICS: 'false', WRANGLER_LOG: 'log',
      WRANGLER_OUTPUT_FILE_PATH: join(this.#output, stage + '.jsonl'),
    })
    await writeFile(join(this.#output, stage + '.log'), result.stdout + result.stderr)
    return result.stdout
  }
}
