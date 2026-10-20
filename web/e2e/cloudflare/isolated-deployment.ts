import { execFile } from 'node:child_process'
import { createHash, randomBytes, randomUUID } from 'node:crypto'
import { mkdir, readFile, rm, writeFile } from 'node:fs/promises'
import { createRequire } from 'node:module'
import { dirname, join, resolve } from 'node:path'
import { fileURLToPath } from 'node:url'
import { promisify } from 'node:util'

const PACKAGE_ROOT = resolve(dirname(fileURLToPath(import.meta.url)), '../../../relay-rust/crates/relay-cloudflare')
const WRANGLER = join(PACKAGE_ROOT, 'node_modules/wrangler/bin/wrangler.js')
const COMMAND_TIMEOUT_MILLISECONDS = 120_000
const SECRET_BYTES = 32
const WORKER_ID_CHARACTERS = 12
const MAXIMUM_COMMAND_OUTPUT_BYTES = 4 * 1024 * 1024

interface HostConfiguration {
  readonly compatibility_date: string
  readonly compatibility_flags: readonly string[]
  readonly migrations: readonly unknown[]
  readonly durable_objects: { readonly bindings: readonly { readonly script_name?: string }[] }
  readonly observability: unknown
}

export interface DeploymentIdentity {
  readonly workerName: string
  readonly url: string
  readonly versionId: string
  readonly deploymentId: string
  readonly revision: string
  readonly startedAt: string
  readonly completedAt: string
}

export interface RelayTarget {
  readonly url: string
  readonly accessKey: string
  readonly testSecret: string
  readonly configPath: string
}

/** Owns only a randomly named Worker; never accepts an existing deployment as a target. */
export class IsolatedDeployment {
  readonly workerName = 'windshare-redeploy-' + randomUUID().replaceAll('-', '').slice(0, WORKER_ID_CHARACTERS)
  readonly #output: string
  readonly #configPath: string
  readonly #secretsPath: string
  readonly #relayConfigPath: string
  #accessKey = ''
  readonly #ownerPassword = randomBytes(SECRET_BYTES).toString('base64url')
  readonly #testSecret = randomBytes(SECRET_BYTES).toString('base64url')
  #configuration: Record<string, unknown> = {}
  #deploymentAttempted = false
  #before: DeploymentIdentity | undefined

  constructor(output: string) {
    this.#output = output
    this.#configPath = join(output, 'wrangler.json')
    this.#secretsPath = join(output, 'secrets.json')
    this.#relayConfigPath = join(output, 'relays.json')
  }

  async provision(): Promise<{ target: RelayTarget; deployment: DeploymentIdentity }> {
    await mkdir(this.#output, { recursive: true })
    const require = createRequire(join(PACKAGE_ROOT, 'package.json'))
    const { unstable_readConfig } = require('wrangler') as {
      unstable_readConfig(args: { config: string }): HostConfiguration
    }
    const host = unstable_readConfig({ config: join(PACKAGE_ROOT, 'wrangler.toml') })
    if (host.durable_objects.bindings.some(binding => binding.script_name)) {
      throw new Error('The isolated run cannot bind to another Worker')
    }
    this.#configuration = {
      name: this.workerName,
      main: join(PACKAGE_ROOT, 'tests/entry.mjs'),
      compatibility_date: host.compatibility_date,
      compatibility_flags: host.compatibility_flags,
      migrations: host.migrations,
      durable_objects: host.durable_objects,
      observability: host.observability,
      workers_dev: true,
      preview_urls: false,
      version_metadata: { binding: 'TEST_VERSION' },
      vars: { CONSOLE_ORIGIN: 'https://console.invalid' },
    }
    // Build before downloading so compilation time cannot consume the interruption window.
    await this.#command('build', 'worker-build', ['--release'])
    await writeFile(this.#secretsPath, JSON.stringify({
      OWNER_PASSWORD: this.#ownerPassword, PREVIEW_TEST_SECRET: this.#testSecret,
    }), { mode: 0o600 })
    this.#before = await this.#deploy('before', ['--secrets-file', this.#secretsPath])
    await rm(this.#secretsPath)
    this.#accessKey = await this.#enrollCredential(this.#before.url)
    await writeFile(this.#relayConfigPath, JSON.stringify({
      profiles: [{ url: this.#before.url, access_key: this.#accessKey }], active_relays: [this.#before.url],
    }), { mode: 0o600 })
    return {
      deployment: this.#before,
      target: {
        url: this.#before.url, accessKey: this.#accessKey,
        testSecret: this.#testSecret, configPath: this.#relayConfigPath,
      },
    }
  }

  async #enrollCredential(base: string): Promise<string> {
    let cookie = ''
    const call = async (action: string, body: unknown) => {
      const response = await fetch(base + '/__test/access', {
        method: 'POST', headers: { Authorization: 'Bearer ' + this.#testSecret,
          'Content-Type': 'application/json', Cookie: cookie },
        body: JSON.stringify({ action, body }), signal: AbortSignal.timeout(COMMAND_TIMEOUT_MILLISECONDS),
        redirect: 'error',
      })
      if (!response.ok) throw new Error('Test enrollment failed: ' + response.status)
      cookie = response.headers.get('Set-Cookie')?.split(';')[0] ?? cookie
      return await response.json() as { enrollment_id: string; user_code: string; access_key: string }
    }
    await call('login', { password: this.#ownerPassword })
    const secret = randomBytes(SECRET_BYTES)
    const pending = await call('enrollments', { device_name: 'Browser recovery scenario',
      claim_digest: createHash('sha256').update(secret).digest('base64url') })
    await call('enrollments/approve', { enrollment_id: pending.enrollment_id, user_code: pending.user_code })
    const claimant = { enrollment_id: pending.enrollment_id, claim_secret: secret.toString('base64url') }
    const credential = await call('enrollments/claim', claimant)
    await call('enrollments/installed', claimant)
    return credential.access_key
  }

  async redeploy() {
    if (!this.#before) throw new Error('Provision the isolated Worker before updating it')
    // Request immediate replacement in this fault scenario without changing
    // production policy. Global propagation still requires runtime observation.
    // Omitted secrets and the unchanged DO binding preserve credentials and state.
    const after = await this.#deploy('after', ['--durable-objects-code-update-mode', 'immediate'])
    if (after.url !== this.#before.url || after.versionId === this.#before.versionId ||
      after.deploymentId === this.#before.deploymentId) {
      throw new Error('Redeployment must change deployment/version identity at the same address')
    }
    return { ...after, durableObjectsCodeUpdateMode: 'immediate' as const }
  }

  async dispose(): Promise<void> {
    try {
      if (this.#deploymentAttempted) {
        await this.#wrangler('cleanup', ['delete', this.workerName])
      }
    } finally {
      await rm(this.#secretsPath, { force: true })
      await rm(this.#relayConfigPath, { force: true })
    }
  }

  async #deploy(stage: string, extra: string[]): Promise<DeploymentIdentity> {
    const revision = randomUUID()
    // A different compiled constant proves a code update, even when Rust is unchanged.
    await writeFile(this.#configPath, JSON.stringify({
      ...this.#configuration, define: { __WINDSHARE_TEST_REVISION__: JSON.stringify(revision) },
    }, null, 2))
    const startedAt = new Date().toISOString()
    this.#deploymentAttempted = true
    await this.#wrangler(stage, ['deploy', ...extra])
    const records = (await readFile(join(this.#output, stage + '.jsonl'), 'utf8'))
      .trim().split('\n').map(line => JSON.parse(line) as {
        type: string; worker_name: string; version_id: string; targets: string[]
      })
    const deployed = records.find(record => record.type === 'deploy')
    if (!deployed || deployed.worker_name !== this.workerName || !deployed.version_id) {
      throw new Error('Wrangler did not record the owned Worker deployment')
    }
    const url = deployed.targets.find(target => target.startsWith('https://'))
    if (!url || new URL(url).hostname.split('.')[0] !== this.workerName) {
      throw new Error('Missing isolated workers.dev address')
    }
    const status = JSON.parse(await this.#wrangler(stage + '-status', ['deployments', 'status', '--json'])) as {
      id: string; versions: { version_id: string; percentage: number }[]
    }
    if (!status.id || status.versions.length !== 1 ||
      status.versions[0]?.version_id !== deployed.version_id || status.versions[0]?.percentage !== 100) {
      throw new Error('The new version must receive all deployment traffic')
    }
    const identity = {
      workerName: this.workerName, url: new URL(url).origin, versionId: deployed.version_id,
      deploymentId: status.id, revision, startedAt, completedAt: new Date().toISOString(),
    }
    await writeFile(join(this.#output, stage + '-identity.json'), JSON.stringify(identity, null, 2))
    console.info(JSON.stringify({ event: 'cloudflare-deployment', stage, ...identity }))
    return identity
  }

  async #wrangler(stage: string, args: string[]): Promise<string> {
    return this.#command(stage, process.execPath, [WRANGLER, ...args, '--config', this.#configPath])
  }

  async #command(stage: string, executable: string, args: string[]): Promise<string> {
    const outputPath = join(this.#output, stage + '.jsonl')
    try {
      const result = await promisify(execFile)(executable, args, {
        cwd: PACKAGE_ROOT,
        env: { ...process.env, CI: 'true', WRANGLER_SEND_METRICS: 'false', WRANGLER_OUTPUT_FILE_PATH: outputPath,
          WRANGLER_LOG: 'log' },
        timeout: COMMAND_TIMEOUT_MILLISECONDS, windowsHide: true, maxBuffer: MAXIMUM_COMMAND_OUTPUT_BYTES,
      })
      await writeFile(join(this.#output, stage + '.log'), result.stdout + result.stderr)
      return result.stdout
    } catch (error) {
      await writeFile(join(this.#output, stage + '.log'), String(error))
      throw error
    }
  }
}
