export const ADMIN_CSP = [
  "default-src 'none'",
  "script-src 'self'",
  "style-src 'self'",
  "connect-src 'self'",
  "img-src 'self'",
  "base-uri 'none'",
  "form-action 'self'",
  "frame-ancestors 'none'",
].join('; ')

export {
  ADMIN_APP_CSS,
  ADMIN_APP_JS,
  ADMIN_FAVICON_SVG,
  ADMIN_HTML,
} from './console-dist/assets.generated'
