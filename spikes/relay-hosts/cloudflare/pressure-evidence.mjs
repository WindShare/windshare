import { createHash } from 'node:crypto';
import { readFile, readdir, writeFile } from 'node:fs/promises';
import { fileURLToPath } from 'node:url';
import { posix } from 'node:path';

// Explicit filenames keep older or unrelated local runs out of the report.
const inputs = process.argv.slice(2);
if (!inputs.length) throw new Error('Pass the pressure evidence filenames to summarize');
const root = new URL('../', import.meta.url);
const directory = new URL('./.evidence/', import.meta.url);
const readJSON = async name => JSON.parse(await readFile(new URL(name, directory), 'utf8'));
const platform = await readJSON('pressure-platform-metrics.json');
const metricRows = platform.response.result.viewer.accounts[0];
const percentile = (values, p) => {
  const sorted = values.filter(Number.isFinite).sort((a, b) => a - b);
  return sorted.length ? sorted[Math.min(sorted.length - 1, Math.floor(sorted.length * p))] : null;
};
function summary(run) {
  const pings = run.healthy_peers.flatMap(peer => peer.record?.pings ?? peer.pings);
  const transfers = run.healthy_peers.flatMap(peer => peer.transfers);
  const dataSteps = run.steps.filter(step => step.burst);
  return {
    run: run.run, mode: run.mode,
    calibration_kind: run.mode === 'calibration' ? (run.calibration_kind ?? 'arraybuffer') : undefined,
    result: run.result,
    reported_memory_sample_max_bytes: percentile(metricRows.durableObjectsPeriodicGroups
      .filter(row => row.dimensions.objectId === run.initial_status.value.object)
      .map(row => row.max.memoryUsageBytes), 1),
    maximum_successful_calibration_bytes: percentile(run.steps
      .filter(s => s.allocation?.status === 200).map(s => s.allocation.value.retained_bytes), 1),
    calibration_boots: [...new Set(run.steps.map(s => s.allocation?.value?.boot)
      .concat(run.calibration_recovery?.value?.boot).filter(Boolean))],
    cumulative_send_returned_mib: run.cumulative_send_returned_bytes / 1048576,
    old_peer_count: dataSteps.length,
    complete_drains: dataSteps.filter(step => step.drain?.frames === run.limits.frames_per_step).length,
    received_mib: dataSteps.reduce((sum, step) => sum + (step.drain?.frames ?? 0), 0) / 16,
    hold_after_send_ms: { min: percentile(dataSteps.map(s => s.hold_after_send_ms), 0),
      max: percentile(dataSteps.map(s => s.hold_after_send_ms), 1) },
    distinct_rust_boots: new Set(dataSteps.flatMap(s => [s.burst?.value?.rust_boot,
      s.status?.value?.rust_boot]).filter(Boolean)).size,
    ping_ms: { p50: percentile(pings.map(p => p.ms), 0.5), p95: percentile(pings.map(p => p.ms), 0.95),
      max: percentile(pings.map(p => p.ms), 1),
      errors: pings.filter(p => p.error && !p.error.includes('probe cleanup')).length,
      cleanup_cancellations: pings.filter(p => p.error?.includes('probe cleanup')).length },
    healthy_echo_mib_per_second: { min: percentile(transfers.map(t => t.echoed_mib_per_second), 0),
      p50: percentile(transfers.map(t => t.echoed_mib_per_second), 0.5),
      max: percentile(transfers.map(t => t.echoed_mib_per_second), 1) },
    baseline: run.baseline, final_healthy: run.final_healthy, new_connection: run.new_connection,
    reset_http_statuses: [...new Set(dataSteps.filter(s => s.reset).map(s => s.reset.status))],
    reset_peer_closures_observed: dataSteps.filter(s => s.healthy_closed_after_reset).length,
    replacements_worked: dataSteps.filter(s => s.replacement).length,
    last_allocation: run.steps.filter(s => s.allocation).at(-1)?.allocation,
    calibration_recovery: run.calibration_recovery,
  };
}
const rawHashes = {};
const runs = await Promise.all(inputs.map(async name => {
  const bytes = await readFile(new URL(name, directory));
  rawHashes[name] = createHash('sha256').update(bytes).digest('hex');
  return JSON.parse(bytes);
}));
function compact(value) {
  if (Array.isArray(value)) return value.map(compact);
  if (!value || typeof value !== 'object') return value;
  const result = {};
  for (const [key, item] of Object.entries(value)) {
    if (key === 'sockets' && Array.isArray(item)) {
      result.socket_count = item.length;
      result.socket_states = item.reduce((counts, socket) => {
        const key = (socket.attachment?.role?.startsWith('slow-') ? 'slow' : 'healthy') + ':' + socket.ready_state;
        counts[key] = (counts[key] ?? 0) + 1;
        return counts;
      }, {});
    } else result[key] = compact(item);
  }
  return result;
}
const sources = {};
async function collect(path, prefix = '') {
  for (const entry of await readdir(path, { withFileTypes: true })) {
    const relative = prefix + entry.name;
    if (['node_modules', '.target', 'target', '.wrangler', '.evidence', 'build', 'evidence'].includes(entry.name)) continue;
    if (entry.isDirectory()) await collect(new URL(entry.name + '/', path), relative + '/');
    else if (/\.(rs|mjs|toml|lock|json)$/.test(entry.name)) {
      sources[relative] = createHash('sha256').update(await readFile(new URL(entry.name, path))).digest('hex');
    }
  }
}
await collect(root);
async function sourceManifest(name) {
  return Object.fromEntries((await readJSON(name)).map(entry => [posix.normalize('cloudflare/' + entry.path), entry.sha256]));
}
const v1 = await sourceManifest('pressure-v1-sources.json');
const v2 = await sourceManifest('pressure-v2-sources.json');
const buildHashes = {};
for (const path of ['build/index.js', 'build/index_bg.wasm', '../native/.target/release/cloudflare-slow-peer.exe']) {
  buildHashes[posix.normalize('cloudflare/' + path)] = createHash('sha256')
    .update(await readFile(new URL(path, import.meta.url))).digest('hex');
}
const deploymentText = await readFile(new URL('pressure-deployment-final.json', directory), 'utf8');
const finalDeployment = JSON.parse(deploymentText.slice(deploymentText.search(/^\{/m)));
const result = {
  experiment: 'Cloudflare retained transport pressure', date: '2026-10-17',
  scope: 'Authenticated isolated Preview; platform probes, not a complete WindShare transfer',
  versions: { rust: '1.99.0', node: process.version, worker: '0.8.7',
    worker_build: '0.8.7', wasm_bindgen: '0.2.129', wrangler: '4.151.1', ws: '8.22.0',
    rustls: '0.23.46', tokio_tungstenite: '0.30.0' },
  deployment: { worker: 'windshare-relay-host-probe', account: 'fb1b5468362a79b5545d660ace3e8f9d',
    preview: 'pressure-93669b45f5ed', namespace: '94eabe8033364e42b9e05f4484fa0349',
    v1: 'c56f2e7e-3f0d-467a-8c80-954c5202f2c1', v2: '827d675a-3207-4d1d-be03-9d46f25bea61',
    final: finalDeployment.deployment.id,
    source_mapping: 'v1: initial smoke, ArrayBuffer, live/close/reset load; v2: heap and Wasm calibration; final: heap-burst and final smoke. v1 initial smoke/calibration/live used v1 client; later runs used v2 client.',
    differences: 'v2 adds heap/Wasm calibration and a defensive drain timestamp fallback. Final adds heap-burst and Clippy-equivalent Rust edits; the final smoke rechecks payload integrity.' },
  source_sha256: sources,
  tested_source_manifests: { v1, v2, final_build: buildHashes },
  platform_metrics: platform,
  resource_outcome_query: await readJSON('pressure-resource-outcomes.json'),
  bounded_checks: await readJSON('pressure-checks.json'),
  cleanup: await readJSON('pressure-cleanup.json'),
  summary: runs.map(summary),
  raw_evidence_sha256: rawHashes,
  runs: runs.map(compact),
  interpretation: {
    physical_release_proven: false, finite_retention_deadline_proven: false,
    cumulative_send_is_not_simultaneous_isolate_memory: true,
    drain_is_an_active_intervention: true,
    observed_exceeded_memory_events: (await readJSON('pressure-resource-outcomes.json')).events
      .filter(event => event.outcome === 'exceededMemory').length,
    memory_limit_calibration: 'Inconclusive for a hard limit: 192 MiB ArrayBuffer/Wasm and heap burst returned successfully; incremental heap reconstructed after 144 MiB; Wasm and heap burst reconstructed after 192 MiB while ArrayBuffer kept its instance. The memory metric responded to calibration, but no exceededMemory outcome was captured.',
    network_scope: 'Current workstation route to Cloudflare SJC; no direct-path or proxy-buffer isolation.',
    retirement_recommendation: 'Continue the Cloudflare route; separate logical generation retirement from host residual admission risk. Preserve live credit and bounded admission. Do not infer immediate physical release or a finite TTL; do not reset a whole share to clean up one slow peer.',
  },
};
const output = new URL('../evidence/2026-10-17-pressure.json', import.meta.url);
await writeFile(output, JSON.stringify(result, null, 2) + '\n');
console.log(JSON.stringify({ output: fileURLToPath(output), summary: result.summary }, null, 2));
