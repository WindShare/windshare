import { FULL_DEPLOYMENT } from './configuration.mjs';

const PUBLIC_FIELDS = new Set(['kind', 'deploymentId', 'namespaceId', 'accessNamespaceId', 'revision', 'serviceId', 'attempt', 'keyId', 'reason']);
const ADDRESS_FIELDS = ['endpoint', 'consoleEndpoint', 'consoleUrl', 'frontendUrl'];

export function deploymentReporter({ githubActions, operationId, mode, write = console.log }) {
  const masked = new Set();
  function mask(value) {
    if (!githubActions || !value || masked.has(value)) return;
    masked.add(value);
    write('::add-mask::' + value.replaceAll('%', '%25').replaceAll('\r', '%0D').replaceAll('\n', '%0A'));
  }
  function maskAddress(value) {
    if (!githubActions || !value) return;
    mask(value);
    mask(new URL(value).hostname);
  }
  return {
    protect(inputs) {
      mask(inputs.ownerPassword);
      mask(inputs.workerName);
      mask(inputs.workerName + '-console');
      if (inputs.route.kind === 'custom-domain') {
        for (const hostname of [inputs.route.hostname, inputs.route.consoleHostname]) maskAddress('https://' + hostname);
      }
    },
    log(event, fields = {}) {
      // Derived workers.dev addresses are only known after route discovery. Register
      // them before Wrangler runs; its diagnostics can also contain these addresses.
      for (const key of ADDRESS_FIELDS) maskAddress(fields[key]);
      const visible = githubActions
        ? Object.fromEntries(Object.entries(fields).filter(([key]) => PUBLIC_FIELDS.has(key))) : fields;
      write(JSON.stringify({ event, operationId, mode, ...visible }));
    },
  };
}

export function deploymentReport(result, { githubActions, mode, revision, outputDirectory }) {
  const heading = ['### WindShare Cloudflare deployment', '', 'Mode: ' + mode, '',
    result.turn?.status === 'incomplete' ? 'TURN setup incomplete: ' + result.turn.reason + ' Rerun deployment to retry.' :
      'Managed TURN is available automatically when enabled in the console. Cloudflare Realtime usage is billed separately.',
    'Local TURN budgets default to unlimited and apply per Engine; they are not account-wide spending caps.', ''];
  const frontendUrl = mode === FULL_DEPLOYMENT ? result.state.endpoint : null;
  const onboarding = 'wind relay onboard ' + result.state.endpoint;
  // A caller-owned destination is private output, independent of public CI logs.
  // The live smoke check uses it for verification and removes it after completion.
  const file = outputDirectory ? {
    directory: outputDirectory,
    data: {
      endpoint: result.state.endpoint, consoleUrl: result.state.consoleEndpoint, onboarding,
      deploymentId: result.state.deploymentId, namespaceId: result.namespaceId, accessNamespaceId: result.accessNamespaceId,
      revision, mode, frontendUrl, turn: result.turn,
    },
  } : null;
  if (githubActions) return {
    file,
    summary: [...heading,
      'Deployment verified. Addresses are available in your Cloudflare dashboard under the Worker’s Domains & Routes.', '',
      'Run `wind relay onboard <relay-url>` and approve the device in the console with your owner password.', '',
      'Deployment identity, enrolled credentials and share storage are preserved.', '',
    ].join('\n'),
  };
  return {
    file,
    summary: [...heading,
      'Relay: ' + result.state.endpoint, '', 'Console: ' + result.state.consoleEndpoint, '',
      frontendUrl ? 'Frontend: ' + frontendUrl : 'Frontend: hosted separately (pass --front-url when sharing).', '',
      'Run this command and approve the device in the console with your owner password:', '',
      '    ' + onboarding, '    wind share <path>', '',
      'Onboarding keeps existing profiles and selects only this private relay. P2P stays enabled.',
      'Updates preserve relay identity, enrolled credentials and share storage. Changing the owner password signs administrators out.', '',
    ].join('\n'),
  };
}
