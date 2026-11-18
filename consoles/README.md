# Management consoles

The node console, network console and public report form support English and Simplified Chinese. The header language selector remembers the choice for that origin. The first visit uses the browser’s preferred supported language, falling back to English. To add a language, register it in `ui/locale.ts` and translate the catalogs under `ui/locales/`, `relay/locales/` and `network/locales/`.

From `consoles/`, run `pnpm install --frozen-lockfile`. Each app uses its host’s real API and administrator cookie.

| App | Local command | Browser address | Backend override |
| --- | --- | --- | --- |
| Node | `pnpm dev:relay` | `http://127.0.0.1:5174/` | `RELAY_BACKEND`, default `http://127.0.0.1:8484` |
| Network | `pnpm dev:network` | `http://127.0.0.1:5175/console`, `/report` | `AUTHORITY_BACKEND`, default `http://127.0.0.1:8787` |

For native pairing, listen on the backend address and set the node’s `endpoint` to `ws://127.0.0.1:5174/v2/ws`. Open its initialization link, then run `wind relay onboard http://127.0.0.1:5174`. Discovery, pairing and WebSocket upgrades pass through Vite. For a local Cloudflare relay, set `CONSOLE_ORIGIN` to the node UI address and use a distinct relay origin. For Authority, configure the local Worker with test signing keys and the browser-visible origin. The proxies preserve Host, Origin and cookies.

Run `pnpm build:relay` or `pnpm build:network` and include the owning crate’s entire `console-dist/` in the same change. Cargo embeds its generated path map, HTML and assets directly. The public report entry imports the shared rendering runtime and its own form.
