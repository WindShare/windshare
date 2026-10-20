import { spawnSync } from 'node:child_process';
import { randomUUID } from 'node:crypto';
import { appendFile, mkdir, mkdtemp, rm, rmdir, writeFile } from 'node:fs/promises';
import { tmpdir } from 'node:os';
import { createRequire } from 'node:module';
import { dirname, join, resolve } from 'node:path';
import { fileURLToPath } from 'node:url';
import { cloudflareClient } from './cloudflare.mjs';
import { consoleConfig, deploymentConfig, FULL_DEPLOYMENT, readDeploymentMode } from './configuration.mjs';
import { deployRelay, readInputs, SECRET_BINDING, STATE_BINDING, validateHostConfig } from './deployment.mjs';

const repositoryRoot = resolve(dirname(fileURLToPath(import.meta.url)), '../../..');
const hostDirectory = join(repositoryRoot, 'relay-rust/crates/relay-cloudflare');
const COMMAND_TIMEOUT_MS = 20 * 60 * 1000;
const operationId = process.env.GITHUB_RUN_ID ?? randomUUID();

function command(program, args, options = {}) {
  const { env, ...commandOptions } = options;
  const result = spawnSync(program, args, {
    cwd: hostDirectory, stdio: 'inherit', timeout: COMMAND_TIMEOUT_MS, windowsHide: true,
    env: { ...process.env, CI: 'true', WRANGLER_SEND_METRICS: 'false', WRANGLER_LOG: 'info', ...env }, ...commandOptions,
  });
  if (result.error || result.status !== 0) throw new Error(program + ' failed (exit ' + (result.status ?? 'unavailable') + ').');
}
async function publish(plan, config, ownerPassword) {
  const configPath = join(hostDirectory, 'wrangler.actions-' + randomUUID() + '.json');
  const args = ['deploy', '--config', configPath, '--keep-vars', '--var', STATE_BINDING + ':' + JSON.stringify(plan.state)];
  let secretDirectory;
  try {
    await writeFile(configPath, JSON.stringify(config), { flag: 'wx' });
    if (ownerPassword !== undefined) {
      if (process.env.GITHUB_ACTIONS === 'true') console.log('::add-mask::' + ownerPassword);
      secretDirectory = await mkdtemp(join(tmpdir(), 'windshare-owner-'));
      const secretsFile = join(secretDirectory, 'secrets.json');
      await writeFile(secretsFile, JSON.stringify({ [SECRET_BINDING]: ownerPassword }), { mode: 0o600, flag: 'wx' });
      args.push('--secrets-file', secretsFile);
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
  const mode = readDeploymentMode(process.env);
  const inputs = readInputs(process.env);
  const require = createRequire(join(hostDirectory, 'package.json'));
  const { experimental_readRawConfig, unstable_readConfig } = require('wrangler');
  const source = { config: join(hostDirectory, 'wrangler.toml') };
  validateHostConfig(unstable_readConfig(source));
  const host = experimental_readRawConfig(source).rawConfig;
  const log = (event, fields) => console.log(JSON.stringify({ event, operationId, mode, ...fields }));
  const client = workerName => cloudflareClient({ accountId: inputs.accountId, workerName, apiToken: process.env.CLOUDFLARE_API_TOKEN });
  log('deployment_started', { workerName: inputs.workerName, revision: process.env.GITHUB_SHA });
  const result = await deployRelay({
    inputs, cloudflare: client(inputs.workerName), consoleCloudflare: client(inputs.workerName + '-console'), log,
    deploy: async plan => {
      if (mode === FULL_DEPLOYMENT) command('pnpm build', [], {
        cwd: join(repositoryRoot, 'web'), shell: true,
        env: { WIND_BUILD_REVISION: process.env.GITHUB_SHA ?? process.env.WIND_BUILD_REVISION },
      });
      await publish(plan, deploymentConfig(host, inputs, mode, plan.state), inputs.ownerPassword);
    },
    deployConsole: plan => publish(plan, consoleConfig(host, inputs, plan.state)),
  });
  const frontendUrl = mode === FULL_DEPLOYMENT ? result.state.endpoint : null;
  const onboarding = 'wind relay onboard ' + result.state.endpoint;
  const outputDirectory = resolve(process.env.WINDSHARE_DEPLOYMENT_OUTPUT ?? join(tmpdir(), 'windshare-deployment'));
  await mkdir(outputDirectory, { recursive: true });
  await writeFile(join(outputDirectory, 'deployment.json'), JSON.stringify({
    endpoint: result.state.endpoint, consoleUrl: result.state.consoleEndpoint, onboarding,
    deploymentId: result.state.deploymentId, namespaceId: result.namespaceId, accessNamespaceId: result.accessNamespaceId,
    revision: process.env.GITHUB_SHA, mode, frontendUrl,
  }, null, 2) + '\n');
  const summary = [
    '### WindShare Cloudflare deployment', '', 'Mode: ' + mode, '',
    'Relay: ' + result.state.endpoint, '', 'Console: ' + result.state.consoleEndpoint, '',
    frontendUrl ? 'Frontend: ' + frontendUrl : 'Frontend: hosted separately (pass --front-url when sharing).', '',
    'Run this command and approve the device in the console with your owner password:', '',
    '    ' + onboarding, '    wind share <path>', '',
    'Onboarding keeps existing profiles and selects only this private relay. P2P stays enabled.',
    'Updates preserve relay identity, enrolled credentials and share storage. Changing the owner password signs administrators out.', '',
  ].join('\n');
  if (process.env.GITHUB_STEP_SUMMARY) await appendFile(process.env.GITHUB_STEP_SUMMARY, summary);
  console.log(summary);
  log('onboarding_ready', { deploymentId: result.state.deploymentId, consoleUrl: result.state.consoleEndpoint, onboarding });
}
main().catch(error => {
  console.error(JSON.stringify({ event: 'deployment_failed', operationId, message: error.message }));
  process.exitCode = 1;
});
