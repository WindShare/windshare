import { createHash } from 'node:crypto';
import { readFile, writeFile } from 'node:fs/promises';
import { fileURLToPath } from 'node:url';
import { formatPublicEvidence } from '../public-evidence.mjs';

const directory = new URL('./.evidence/', import.meta.url);
const manifestName = process.argv[2];
if (!manifestName) throw new Error('Pass an explicit run manifest filename from .evidence');
const read = async name => JSON.parse(await readFile(new URL(name, directory), 'utf8'));
const manifest = await read(manifestName);
const hashes = {};
async function evidence(name) {
  const bytes = await readFile(new URL(name, directory));
  hashes[name] = createHash('sha256').update(bytes).digest('hex');
  return JSON.parse(bytes);
}
function distribution(values) {
  const sorted = values.filter(Number.isFinite).sort((a, b) => a - b);
  const pick = p => sorted.length ? sorted[Math.min(sorted.length - 1, Math.floor(sorted.length * p))] : null;
  return { count: sorted.length, min: pick(0), p50: pick(0.5), p95: pick(0.95), max: pick(1) };
}
function status(response) {
  if (!response?.value) return response;
  const { sockets, ...value } = response.value;
  return { ...response, value: { ...value, socket_count: sockets?.length,
    socket_states: sockets?.reduce((counts, socket) => {
      const key = (socket.attachment?.role?.startsWith('slow-') ? 'slow:' : 'healthy:') + socket.ready_state;
      counts[key] = (counts[key] ?? 0) + 1;
      return counts;
    }, {}) } };
}
function healthy(peers) {
  const pings = peers.flatMap(peer => peer.pings ?? []);
  const transfers = peers.flatMap(peer => peer.transfers ?? []);
  return { ping_ms: distribution(pings.map(ping => ping.ms)),
    errors: pings.filter(ping => ping.error && !ping.error.includes('probe cleanup')),
    cleanup_cancellations: pings.filter(ping => ping.error?.includes('probe cleanup')).length,
    transfers: distribution(transfers.map(transfer => transfer.echoed_mib_per_second)),
    connections: peers.map(({ pings, transfers, ...peer }) => peer) };
}
function compact(run) {
  const steps = run.steps.map(step => ({ ...step, status: status(step.status) }));
  const dataSteps = steps.filter(step => step.burst?.status === 200);
  const bytes = run.cumulative_submitted_bytes ?? run.cumulative_send_returned_bytes;
  return {
    run: run.run, endpoint: run.endpoint, mode: run.mode, policy: run.policy, disposition: run.disposition,
    scope: run.scope, started_at: run.started_at, finished_at: run.finished_at,
    limits: run.limits, result: run.result, error: run.error,
    physical_release_proven: false,
    summary: { submissions: dataSteps.length, submitted_mib: bytes / 1048576,
      received_mib: dataSteps.reduce((sum, step) => sum + (step.drain?.frames ?? 0), 0) * run.limits.frame_bytes / 1048576,
      complete_drains: dataSteps.filter(step => step.drain?.frames === run.limits.frames_per_step).length,
      abnormal_drains: dataSteps.filter(step => step.drain?.terminal?.kind === 'error').length,
      drain_errors: dataSteps.filter(step => step.drain_error).length,
      abortive_client_closes: dataSteps.filter(step => step.terminal?.terminal?.kind === 'abortive_client_close').length,
      hold_after_send_ms: distribution(dataSteps.map(step => step.hold_after_send_ms)),
      maximum_reported_socket_count: Math.max(0, ...steps.map(step => step.status?.value?.socket_count ?? 0)),
      admission_refusals: run.refusals?.length ?? 0,
      recovered_refusals: run.refusals?.filter(refusal => refusal.recovered_automatically).length ?? 0 },
    healthy: healthy(run.healthy_peers),
    initial_status: status(run.initial_status),
    hold_samples: run.hold_samples?.map(sample => ({ ...sample, status: status(sample.status) })),
    before_hold: status(run.before_hold), after_hold: status(run.after_hold),
    before_final_hold: status(run.before_final_hold), after_final_hold: status(run.after_final_hold),
    final_status: status(run.final_status),
    baseline: run.baseline, final_healthy: run.final_healthy, new_connection: run.new_connection,
    final_sibling: run.final_sibling, sibling_after_load: run.sibling_after_load,
    refusals: run.refusals?.map(refusal => ({ ...refusal, before: status(refusal.before),
      after_wait: status(refusal.after_wait) })),
    events_before_drain: run.events_before_drain, events_after_cleanup: run.events_after_cleanup,
    steps,
  };
}
const deployments = await Promise.all(manifest.deployments.map(async name => {
  const bytes = await readFile(new URL(name, directory));
  hashes[name] = createHash('sha256').update(bytes).digest('hex');
  const text = bytes.toString();
  const deployment = JSON.parse(text.slice(text.indexOf('{')));
  return { preview: deployment.preview.slug, url: deployment.preview.urls[0],
    deployment: deployment.deployment.id, created_on: deployment.deployment.created_on,
    immutable_url: deployment.deployment.urls[0], namespaces: deployment.deployment.env };
}));
const runs = await Promise.all(manifest.runs.map(async name => ({ evidence_file: name, ...compact(await evidence(name)) })));
const result = {
  experiment: 'Cloudflare retirement, retention and lost-feedback follow-up',
  date: manifest.date,
  scope: 'Isolated authenticated Previews; platform policy probes plus the real kernel with canonical fixture clients',
  versions: { node: process.version, worker: '0.8.7', wrangler: '4.151.1' },
  deployments,
  source_manifests: await Promise.all(manifest.source_manifests.map(async file => ({ file, record: await evidence(file) }))),
  source_mapping: manifest.source_mapping,
  kernel_feedback_loss: await evidence(manifest.kernel),
  policy_recovery: await evidence(manifest.recovery),
  metrics: await evidence(manifest.metrics),
  resource_outcomes: await evidence(manifest.outcomes),
  close_callbacks: await evidence(manifest.close_callbacks),
  cleanup: await evidence(manifest.cleanup),
  runs,
  raw_evidence_sha256: hashes,
  interpretation: manifest.interpretation,
};
const output = new URL('../evidence/' + manifest.date + '-retirement.json', import.meta.url);
await writeFile(output, formatPublicEvidence(result));
console.log(JSON.stringify({ output: fileURLToPath(output),
  runs: runs.map(run => ({ run: run.run, policy: run.policy, disposition: run.disposition, mode: run.mode,
    result: run.result, ...run.summary, ping_errors: run.healthy.errors.length })) }, null, 2));
