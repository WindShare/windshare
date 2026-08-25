import { createReadStream } from 'node:fs'
import { stat, writeFile } from 'node:fs/promises'
import { createServer } from 'node:http'
import { extname, resolve, sep } from 'node:path'

const options = parseArguments(process.argv.slice(2))
const webRoot = resolve(options.webRoot)
const workloadPath = resolve(options.workload)
const mime = new Map([
  ['.html', 'text/html; charset=utf-8'],
  ['.mjs', 'text/javascript; charset=utf-8'],
  ['.json', 'application/json; charset=utf-8'],
])

function parseArguments(argv) {
  const values = { host: '127.0.0.1', port: 0, webRoot: null, workload: null, readyFile: null }
  for (let index = 0; index < argv.length; index += 1) {
    const argument = argv[index]
    if (argument === '--host') values.host = argv[++index]
    else if (argument === '--port') values.port = Number.parseInt(argv[++index], 10)
    else if (argument === '--web-root') values.webRoot = argv[++index]
    else if (argument === '--workload') values.workload = argv[++index]
    else if (argument === '--ready-file') values.readyFile = argv[++index]
    else throw new Error(`Unknown argument: ${argument}`)
  }
  for (const name of ['webRoot', 'workload', 'readyFile']) {
    if (!values[name]) throw new Error(`Missing required --${name.replace(/[A-Z]/g, value => `-${value.toLowerCase()}`)}`)
  }
  return values
}

function webPath(pathname) {
  const requested = pathname === '/' ? '/scripts/browser-evidence/fsa-small-file/native-probe/index.html' : pathname
  const candidate = resolve(webRoot, decodeURIComponent(requested).replace(/^[/\\]+/, ''))
  const root = webRoot.toLowerCase()
  const folded = candidate.toLowerCase()
  return folded === root || folded.startsWith(`${root}${sep}`) ? candidate : null
}

const server = createServer(async (request, response) => {
  try {
    if (request.method !== 'GET' && request.method !== 'HEAD') {
      response.writeHead(405, { Allow: 'GET, HEAD' })
      response.end()
      return
    }
    const url = new URL(request.url ?? '/', 'http://127.0.0.1')
    if (url.pathname === '/healthz') {
      response.writeHead(200, { 'Content-Type': 'application/json; charset=utf-8' })
      response.end('{"ok":true}')
      return
    }
    const path = url.pathname === '/canonical-workload.json' ? workloadPath : webPath(url.pathname)
    if (path === null || !(await stat(path)).isFile()) throw new Error('not found')
    response.writeHead(200, {
      'Cache-Control': 'no-store',
      'Content-Type': mime.get(extname(path).toLowerCase()) ?? 'application/octet-stream',
    })
    if (request.method === 'HEAD') response.end()
    else createReadStream(path).pipe(response)
  } catch {
    response.writeHead(404, { 'Content-Type': 'text/plain; charset=utf-8' })
    response.end('Not found')
  }
})

await new Promise((resolveListen, rejectListen) => {
  server.once('error', rejectListen)
  server.listen(options.port, options.host, resolveListen)
})
const address = server.address()
if (address === null || typeof address === 'string') throw new Error('Evidence server has no TCP address')
await writeFile(options.readyFile, `${JSON.stringify({
  host: options.host,
  port: address.port,
  pid: process.pid,
  startedAt: new Date().toISOString(),
}, null, 2)}\n`, { encoding: 'utf8', flag: 'wx' })

const close = () => server.close(() => process.exit(0))
process.once('SIGINT', close)
process.once('SIGTERM', close)
