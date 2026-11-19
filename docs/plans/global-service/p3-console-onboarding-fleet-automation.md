# P3 — Console onboarding and fleet automation

Part of the [Global Network Plan](global-network.md#6-phases).

Adding a server takes one command and one approval.

- `wsrelay` gains built-in ACME, including Let's Encrypt IP certificates for hosts without a domain,
  so an overseas or community node is one binary plus one config file. IP certificates last 160 hours:
  follow ACME Renewal Information (ARI), with renewal at half-life as fallback. Persist certificates
  and retry renewals with backoff; keep serving with the current valid certificate and reload replacements
  without interrupting sessions. Expose expiry and renewal failures in node health. Mainland servers
  keep the reverse proxy, which also serves the frontend on port 443.
- Join flow: the console's "Add node" takes name, zone, region, and capacity and returns a one-line
  install command with a one-time join token. The node installs, generates its key, obtains a
  certificate, and binds with the token. Once vantage-point probes succeed it enters as `canary`,
  and it becomes `active` after a healthy period. The console creates DNS records through the
  provider API: Cloudflare for `windshare.cc`, DNSPod or Aliyun for the filed domain. A new
  mainland provider also needs filing access.
- Directory signing moves into the authority: every console change publishes a new serial, and
  frontends and nodes serve the latest signed directory from the authority with caching. The
  repository keeps an exported copy for history, never as a gate.
- Health: CN-A, CN-B, and one overseas node probe enabled services (TCP, WebSocket upgrade with
  each official Origin for relays, STUN binding for STUN hosts) and report to the authority;
  status changes publish as new serials. Nodes do not probe each other.
- Removing marks a node `draining` until its live shares end, then deletes it. Updating walks
  official nodes one at a time through the existing install helper (verify, restart, health check,
  rollback), draining each first.
- Traces: join steps keyed by node ID and token, probe results per vantage point, and directory
  publications (generation, serial, change, mirrors).
