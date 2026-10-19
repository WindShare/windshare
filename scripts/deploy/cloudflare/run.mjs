import { spawnSync } from 'node:child_process';
import { randomUUID } from 'node:crypto';
import { appendFile, mkdir, mkdtemp, rm, rmdir, writeFile } from 'node:fs/promises';
import { tmpdir } from 'node:os';
import { createRequire } from 'node:module';
import { dirname, join, resolve } from 'node:path';
import { fileURLToPath } from 'node:url';
import { cloudflareClient } from './cloudflare.mjs';
import { deployRelay, readInputs, SECRET_BINDING, STATE_BINDING, validateHostConfig } from './deployment.mjs';
import { deploymentConfig } from './routing.mjs';

const repositoryRoot = resolve(dirname(fileURLToPath(import.meta.url)), '../../..');
const hostDirectory = join(repositoryRoot, 'relay-rust/crates/relay-cloudflare');
const COMMAND_TIMEOUT_MS = 20 * 60 * 1000;
const operationId = process.env.GITHUB_RUN_ID ?? randomUUID();

function command(program, args, options = {}) {
  const result = spawnSync(program, args, {
    cwd: hostDirectory, stdio: 'inherit', timeout: COMMAND_TIMEOUT_MS, windowsHide: true,
    env: { ...process.env, CI: 'true', WRANGLER_SEND_METRICS: 'false', WRANGLER_LOG: 'info' },
    ...options,
  });
  // Child-process exception objects can contain stdin, including registration
  // authority. Report only exit information, never serialize the spawn result.
  if (result.error || result.status !== 0) throw new Error(`${program} failed (exit ${result.status ?? 'unavailable'}).`);
  return result.stdout;
}

async function publish(plan, config) {
  const configPath = join(hostDirectory, `wrangler.actions-${randomUUID()}.json`);
  const args = ['deploy', '--config', configPath, '--keep-vars'];
  let secretDirectory;
  try {
    await writeFile(configPath, JSON.stringify(config), { flag: 'wx' });
    if (plan.kind === 'create') {
      if (process.env.GITHUB_ACTIONS === 'true') console.log(`::add-mask::${plan.credential}`);
      secretDirectory = await mkdtemp(join(tmpdir(), 'windshare-registration-'));
      const secretsFile = join(secretDirectory, 'secrets.json');
      await writeFile(secretsFile, JSON.stringify({ [SECRET_BINDING]: plan.credential }), { mode: 0o600, flag: 'wx' });
      args.push('--secrets-file', secretsFile, '--var', `${STATE_BINDING}:${JSON.stringify(plan.state)}`);
    }
    command(process.execPath, [join(hostDirectory, 'node_modules/wrangler/bin/wrangler.js'), ...args]);
  } finally {
    await rm(configPath, { force: true });
    if (secretDirectory) {
      await rm(join(secretDirectory, 'secrets.json'), { force: true });
      await rmdir(secretDirectory);
    }
  }
}

async function main() {
  const inputs = readInputs(process.env);
  const require = createRequire(join(hostDirectory, 'package.json'));
  const { experimental_readRawConfig, unstable_readConfig } = require('wrangler');
  const source = { config: join(hostDirectory, 'wrangler.toml') };
  validateHostConfig(unstable_readConfig(source));
  const config = deploymentConfig(experimental_readRawConfig(source).rawConfig, inputs);
  const log = (event, fields) => console.log(JSON.stringify({ event, operationId, ...fields }));
  const client = cloudflareClient({ ...inputs, apiToken: process.env.CLOUDFLARE_API_TOKEN });
  log('deployment_started', { workerName: inputs.workerName, revision: process.env.GITHUB_SHA });
  const result = await deployRelay({
    inputs, cloudflare: client, log,
    encrypt: (plaintext, recipient) => command('age', ['--encrypt', '--armor', '--recipient', recipient], {
      stdio: ['pipe', 'pipe', 'inherit'], input: plaintext, encoding: 'utf8',
    }).replaceAll('\r\n', '\n'),
    deploy: plan => publish(plan, config),
  });
  const outputDirectory = resolve(process.env.WINDSHARE_DEPLOYMENT_OUTPUT ?? join(tmpdir(), 'windshare-relay-config'));
  await mkdir(outputDirectory, { recursive: true });
  await writeFile(join(outputDirectory, 'relay.json.age'), result.state.encryptedConfig);
  await writeFile(join(outputDirectory, 'deployment.json'), JSON.stringify({
    endpoint: result.state.endpoint, deploymentId: result.state.deploymentId,
    namespaceId: result.namespaceId, revision: process.env.GITHUB_SHA,
  }, null, 2) + '\n');
  if (process.env.GITHUB_STEP_SUMMARY) {
    await appendFile(process.env.GITHUB_STEP_SUMMARY, [
      '### WindShare private relay', '',
      `Address: ${result.state.endpoint}`, '',
      `Deployment: \`${result.state.deploymentId}\` (${result.kind})`, '',
      'Download the **windshare-relay-config** artifact, extract it, then run:', '',
      '\`\`\`sh',
      'age --decrypt -i windshare.key relay.json.age | wind relay import -',
      '\`\`\`', '',
      'Import replaces the complete allowed relay set. Ordinary sharing now uses this private relay.',
      'Keep your local age key. Updates preserve this address, registration credential and share storage.',
      'A completed deployment does not mean every active object has switched versions yet.', '',
    ].join('\n'));
  }
  log('configuration_exported', { deploymentId: result.state.deploymentId, outputDirectory });
}

// Keep this entry point separate from the injected controller used by offline tests.
main().catch(error => {
  console.error(JSON.stringify({ event: 'deployment_failed', operationId, message: error.message }));
  process.exitCode = 1;
});
