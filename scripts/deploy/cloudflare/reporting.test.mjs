import assert from 'node:assert/strict';
import test from 'node:test';
import { deploymentReport, deploymentReporter } from './reporting.mjs';

const inputs = { workerName: 'private-relay', ownerPassword: 'private-owner-password', route: { kind: 'workers-dev' } };
const result = {
  state: { deploymentId: 'deployment-1', endpoint: 'https://private-relay.owner.workers.dev',
    consoleEndpoint: 'https://private-relay-console.owner.workers.dev' },
  namespaceId: 'shares-1', accessNamespaceId: 'access-1',
};

test('Actions masks derived addresses before logging and retains only operational identifiers', () => {
  const lines = [], reporter = deploymentReporter({ githubActions: true, operationId: 'run-1', mode: 'full', write: line => lines.push(line) });
  reporter.protect(inputs);
  reporter.log('deployment_prepared', { kind: 'update', ...result.state, extraAddress: result.state.endpoint });
  const masks = lines.filter(line => line.startsWith('::add-mask::')).map(line => line.slice('::add-mask::'.length));
  for (const value of [inputs.workerName, inputs.ownerPassword, ...[result.state.endpoint, result.state.consoleEndpoint].flatMap(url => [url, new URL(url).hostname])]) {
    assert.ok(masks.includes(value));
  }
  assert.deepEqual(JSON.parse(lines.at(-1)), {
    event: 'deployment_prepared', operationId: 'run-1', mode: 'full', kind: 'update', deploymentId: 'deployment-1',
  });
  reporter.log('deployment_verified', { ...result.state, namespaceId: result.namespaceId, accessNamespaceId: result.accessNamespaceId });
  assert.equal(JSON.parse(lines.at(-1)).namespaceId, result.namespaceId);
  assert.equal(JSON.parse(lines.at(-1)).accessNamespaceId, result.accessNamespaceId);
});

test('custom hostnames and multiline secrets are masked before command execution', () => {
  const lines = [], reporter = deploymentReporter({ githubActions: true, write: line => lines.push(line) });
  reporter.protect({ ...inputs, ownerPassword: 'owner%\r\npassword', route: {
    kind: 'custom-domain', hostname: 'relay.private.example', consoleHostname: 'console.relay.private.example',
  } });
  for (const value of ['https://relay.private.example', 'relay.private.example', 'https://console.relay.private.example', 'console.relay.private.example', 'owner%25%0D%0Apassword']) {
    assert.ok(lines.includes('::add-mask::' + value));
  }
  assert.ok(lines.every(line => !/[\r\n]/.test(line)));
});

test('Actions reports contain no address-bearing artifact or summary in either deployment mode', () => {
  for (const mode of ['full', 'relay-only']) {
    const report = deploymentReport(result, { githubActions: true, mode, revision: 'revision-1' });
    assert.equal(report.file, null);
    for (const url of [result.state.endpoint, result.state.consoleEndpoint]) {
      assert.ok(!report.summary.includes(new URL(url).hostname));
    }
  }
});

test('an explicit private output destination supports CI smoke verification without changing the public summary', () => {
  const options = { githubActions: true, mode: 'full', revision: 'revision-1' };
  const publicReport = deploymentReport(result, options);
  const smokeReport = deploymentReport(result, { ...options, outputDirectory: '/private/smoke-output' });
  assert.equal(smokeReport.file.directory, '/private/smoke-output');
  assert.equal(smokeReport.file.data.endpoint, result.state.endpoint);
  assert.equal(smokeReport.file.data.consoleUrl, result.state.consoleEndpoint);
  assert.equal(smokeReport.summary, publicReport.summary);
});

test('local deployment retains addresses for onboarding and smoke tests', () => {
  const lines = [], reporter = deploymentReporter({ githubActions: false, write: line => lines.push(line) });
  reporter.protect(inputs);
  reporter.log('deployment_prepared', result.state);
  assert.equal(lines.length, 1);
  assert.equal(JSON.parse(lines[0]).endpoint, result.state.endpoint);
  for (const mode of ['full', 'relay-only']) {
    const report = deploymentReport(result, { githubActions: false, mode, revision: 'revision-1', outputDirectory: '/private/local-output' });
    assert.equal(report.file.data.endpoint, result.state.endpoint);
    assert.equal(report.file.data.consoleUrl, result.state.consoleEndpoint);
    assert.equal(report.file.data.frontendUrl, mode === 'full' ? result.state.endpoint : null);
    assert.ok(report.summary.includes(report.file.data.onboarding));
  }
});
