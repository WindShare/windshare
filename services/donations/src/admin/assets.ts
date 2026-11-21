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

export const ADMIN_HTML = `<!doctype html>
<html lang="en">
<head>
  <meta charset="utf-8">
  <meta name="viewport" content="width=device-width, initial-scale=1">
  <meta name="robots" content="noindex, nofollow">
  <title>WindShare Donations Admin</title>
  <link rel="stylesheet" href="/admin/app.css">
</head>
<body>
  <main id="app" class="admin-shell">
    <h1>WindShare Donations Admin</h1>
    <p id="status">Scaffold ready.</p>
  </main>
  <script src="/admin/app.js" defer></script>
</body>
</html>
`

export const ADMIN_APP_CSS = `:root {
  color-scheme: light dark;
  font-family: Inter, ui-sans-serif, system-ui, -apple-system, sans-serif;
}
body {
  margin: 0;
  padding: 2rem;
}
.admin-shell {
  max-width: 960px;
  margin: 0 auto;
}
`

export const ADMIN_APP_JS = `(() => {
  const status = document.getElementById('status')
  if (status) status.textContent = 'Admin moderation endpoints are not enabled yet.'
})()
`
