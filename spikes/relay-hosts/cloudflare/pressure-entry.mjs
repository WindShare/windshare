import { DurableObject } from 'cloudflare:workers';
import { PressureSockets } from './build/index.js';

const MIB = 1024 * 1024;
const CALIBRATION_STEP_BYTES = 16 * MIB;
const MAX_CALIBRATION_STEPS = 12;
const DOUBLE_BYTES = 8;
const WASM_PAGE_BYTES = 65_536;

// Keep deliberate host termination outside a live Rust future.
export class PressureProbe extends DurableObject {
  constructor(ctx, env) {
    super(ctx, env);
    this.boot = crypto.randomUUID();
    this.rust = new PressureSockets(ctx, env);
    this.retained = [];
  }

  async fetch(request) {
    const url = new URL(request.url);
    const action = url.pathname.split('/').at(-1);
    if (request.method === 'GET' && action === 'outer-status') {
      return Response.json({ boot: this.boot, retained_bytes: this.retained.length * CALIBRATION_STEP_BYTES });
    }
    if (request.method === 'POST' && action === 'calibrate') {
      const { kind = 'heap' } = await request.json();
      if (!['heap', 'heap-burst', 'arraybuffer', 'wasm'].includes(kind)) return new Response('invalid calibration kind', { status: 400 });
      const attempts = (await this.ctx.storage.get('calibration-attempts')) ?? 0;
      const allocations = kind === 'heap-burst' ? MAX_CALIBRATION_STEPS : 1;
      if (attempts + allocations > MAX_CALIBRATION_STEPS) return new Response('calibration bound exceeded', { status: 409 });
      await this.ctx.storage.put('calibration-attempts', attempts + allocations);
      await this.ctx.storage.sync();
      // A timer prevents normal hibernation from clearing the retained-memory control.
      this.keepAlive ??= setInterval(() => {}, 1000);
      console.log({ probe: 'pressure', event: 'calibration_attempt', boot: this.boot,
        object: this.ctx.id.toString(), attempt: attempts + 1,
        retained_before_bytes: this.retained.length * CALIBRATION_STEP_BYTES });
      for (let step = 0; step < allocations; step++) {
      if (kind === 'heap' || kind === 'heap-burst') {
        // A non-integer value forces a retained V8 double array, unlike external
        // ArrayBuffer backing stores whose enforcement must be measured separately.
        this.retained.push(new Array(CALIBRATION_STEP_BYTES / DOUBLE_BYTES).fill(attempts + 0.25));
      } else if (kind === 'wasm') {
        const memory = new WebAssembly.Memory({ initial: CALIBRATION_STEP_BYTES / WASM_PAGE_BYTES });
        new Uint8Array(memory.buffer).fill(attempts + 1);
        this.retained.push(memory);
      } else {
        const bytes = new Uint8Array(CALIBRATION_STEP_BYTES);
        bytes.fill(attempts + 1);
        this.retained.push(bytes);
      }
      }
      return Response.json({ boot: this.boot, attempt: attempts + allocations,
        retained_bytes: this.retained.length * CALIBRATION_STEP_BYTES });
    }
    if (request.method === 'POST' && action === 'release-calibration') {
      clearInterval(this.keepAlive);
      this.retained = [];
      return Response.json({ released_references: true });
    }
    if (request.method === 'POST' && action === 'reset') {
      url.pathname = url.pathname.replace(/\/reset$/, '/prepare-reset');
      const prepared = await this.rust.fetch(new Request(url, request));
      if (!prepared.ok) return prepared;
      this.ctx.abort('bounded pressure experiment reset', { retryAlarm: false });
    }
    return this.rust.fetch(request);
  }

  webSocketMessage(...args) { return this.rust.webSocketMessage(...args); }
  webSocketClose(...args) { return this.rust.webSocketClose(...args); }
  webSocketError(...args) { return this.rust.webSocketError(...args); }
}
