import { createHash } from 'node:crypto';
import { readdir, readFile } from 'node:fs/promises';

const root = new URL('../../../', import.meta.url);
async function files(directory) {
  const entries = await readdir(new URL(directory, root), { withFileTypes: true });
  const groups = await Promise.all(entries.map(entry => entry.isDirectory()
    ? files(directory + entry.name + '/')
    : [directory + entry.name]));
  return groups.flat();
}
export async function sourceState() {
  const paths = [
    'spikes/relay-hosts/cloudflare/Cargo.toml', 'spikes/relay-hosts/cloudflare/Cargo.lock',
    'spikes/relay-hosts/cloudflare/package.json', 'spikes/relay-hosts/cloudflare/package-lock.json',
    'spikes/relay-hosts/cloudflare/recovery-entry.mjs', 'spikes/relay-hosts/cloudflare/wrangler.toml',
    'spikes/relay-hosts/cloudflare/checkpoint-probe.mjs', 'spikes/relay-hosts/cloudflare/checkpoint-evidence.mjs',
    'relay-rust/Cargo.toml', 'relay-rust/crates/relay-core/Cargo.toml',
    'core/testvectors/v2-session.json',
    ...await files('spikes/relay-hosts/cloudflare/src/'),
    ...await files('relay-rust/crates/relay-core/src/'),
  ].sort();
  const hashes = await Promise.all(paths.map(async path => [
    path, createHash('sha256').update(await readFile(new URL(path, root))).digest('hex'),
  ]));
  const manifest = Object.fromEntries(hashes);
  return { sha256: createHash('sha256').update(JSON.stringify(manifest)).digest('hex'), files: manifest };
}
