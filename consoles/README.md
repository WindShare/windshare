# Node management console

From `consoles/`, run `pnpm install --frozen-lockfile`, then `pnpm dev:relay`. Open `http://127.0.0.1:5174/`. Vite uses `RELAY_BACKEND` (default `http://127.0.0.1:8484`) for the real node API.

For native pairing, listen on the backend address and set the node’s `endpoint` to `ws://127.0.0.1:5174/v2/ws`. Open its initialization link, then run `wind relay onboard http://127.0.0.1:5174`. Discovery, pairing and WebSocket upgrades pass through Vite. For a local Cloudflare relay, set `CONSOLE_ORIGIN` to the browser-visible address and use a distinct relay origin.

Run `pnpm build:relay` and include the entire `relay-rust/console/console-dist/` in the same change. Both hosts embed its generated path map, HTML and assets through the shared crate.
