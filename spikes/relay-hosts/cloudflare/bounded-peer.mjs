import { spawn } from 'node:child_process';
import { createInterface } from 'node:readline';
import { fileURLToPath } from 'node:url';

// Keep the receive-window control outside Node: pausing ws does not cap the OS buffer.
export async function boundedPeer(url, token, options = {}) {
  const binary = new URL('../native/.target/release/cloudflare-slow-peer' +
    (process.platform === 'win32' ? '.exe' : ''), import.meta.url);
  const child = spawn(fileURLToPath(binary), [], { stdio: ['pipe', 'pipe', 'pipe'], windowsHide: true });
  const messages = [];
  let failure;
  let stderr = '';
  child.on('error', error => { failure = error; });
  child.stderr.on('data', bytes => {
    stderr = (stderr + bytes.toString()).slice(0, 2000);
    failure = new Error(stderr);
  });
  child.stdin.on('error', error => { failure = error; });
  const lines = createInterface({ input: child.stdout });
  lines.on('line', line => {
    try { messages.push(JSON.parse(line)); } catch { failure = new Error('invalid child response'); }
  });
  const exited = new Promise(resolve => child.once('close', resolve));
  async function wait(event, timeout) {
    const deadline = performance.now() + timeout;
    while (performance.now() < deadline) {
      if (failure) throw failure;
      const message = messages.find(value => value.event === event);
      if (message) return message;
      if (child.exitCode !== null) throw new Error('slow peer exited before ' + event);
      await new Promise(resolve => setTimeout(resolve, 25));
    }
    throw new Error('bounded slow peer wait expired: ' + event);
  }
  async function terminate() {
    child.kill();
    await exited;
    lines.close();
  }
  child.stdin.write(JSON.stringify({ ...options, url, token }) + '\n');
  try {
    const ready = await wait('ready', 20_000);
    return { ready, terminate, async resume() {
      child.stdin.write('resume\n');
      return wait('terminal', 25_000);
    } };
  } catch (error) {
    await terminate();
    throw error;
  }
}
