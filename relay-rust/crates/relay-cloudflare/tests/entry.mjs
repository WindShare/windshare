// Preview-only fault injection around the production Rust host; no protocol decisions.
import { DurableObject } from "cloudflare:workers";
import RustWorker, { ShareRelayObject as RustShareRelay } from "../build/index.js";

function authorized(request, env) {
  return !!env.PREVIEW_TEST_SECRET
    && request.headers.get("Authorization") === "Bearer " + env.PREVIEW_TEST_SECRET;
}

export class ShareRelayObject extends DurableObject {
  constructor(ctx, env) {
    super(ctx, env);
    this.boot = crypto.randomUUID();
    this.rust = new RustShareRelay(ctx, env);
  }
  async fetch(request) {
    const url = new URL(request.url);
    if (url.pathname.startsWith("/__test/")) {
      if (!authorized(request, this.env)) return new Response("unauthorized", { status: 401 });
      if (url.pathname === "/__test/status" && request.method === "GET") {
        return Response.json({
          boot: this.boot,
          now: Date.now(),
          sockets: this.ctx.getWebSockets().map(socket => ({
            tags: this.ctx.getTags(socket), state: socket.readyState,
            attachmentBytes: socket.deserializeAttachment()?.byteLength ?? 0,
          })),
          alarm: await this.ctx.storage.getAlarm(),
        });
      }
      if (url.pathname === "/__test/route-mismatch" && request.method === "GET") {
        // Model another public hostname resolving to the same Worker/object.
        url.hostname = "alternate.invalid";
        url.pathname = "/v2/ws";
        return this.rust.fetch(new Request(url, { headers: { Upgrade: "websocket" } }));
      }
      if (url.pathname === "/__test/reset" && request.method === "POST") {
        await this.ctx.storage.sync();
        this.ctx.abort("lifecycle Preview fault injection", { retryAlarm: false });
      }
      return new Response("unknown test operation", { status: 404 });
    }
    return this.rust.fetch(request);
  }
  alarm(...args) { return this.rust.alarm(...args); }
  webSocketMessage(...args) { return this.rust.webSocketMessage(...args); }
  webSocketClose(...args) { return this.rust.webSocketClose(...args); }
  webSocketError(...args) { return this.rust.webSocketError(...args); }
}

export default class PreviewEntrypoint extends RustWorker {
  async fetch(request) {
    const url = new URL(request.url);
    if (!url.pathname.startsWith("/__test/")) return super.fetch(request);
    if (!authorized(request, this.env)) return new Response("unauthorized", { status: 401 });
    const shares = url.searchParams.getAll("share_id");
    if (shares.length !== 1 || !/^[A-Za-z0-9_-]{16}$/.test(shares[0])) {
      return new Response("invalid route", { status: 400 });
    }
    return this.env.SHARES.getByName(shares[0]).fetch(request);
  }
}
