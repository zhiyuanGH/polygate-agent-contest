import { spawn, spawnSync } from 'node:child_process'
import { mkdtemp, rm } from 'node:fs/promises'
import { tmpdir } from 'node:os'
import { dirname, join, resolve } from 'node:path'
import { fileURLToPath } from 'node:url'

const webDir = resolve(dirname(fileURLToPath(import.meta.url)), '..')
const rootDir = resolve(webDir, '..')
const goCandidates = [
  process.env.POLYGATE_GO_BIN,
  process.platform === 'win32' && process.env.LOCALAPPDATA
    ? join(process.env.LOCALAPPDATA, 'PolyGateToolchains', 'go', 'bin', 'go.exe')
    : undefined,
  'go',
].filter(Boolean)

function findGo() {
  for (const bin of goCandidates) {
    const result = spawnSync(bin, ['version'], { stdio: 'ignore' })
    if (result.status === 0) return bin
  }
  throw new Error('Go 1.27 is required. Install it or set POLYGATE_GO_BIN to the go executable.')
}

async function checkAPI() {
  try {
    const response = await fetch('http://127.0.0.1:8080/healthz', { signal: AbortSignal.timeout(1000) })
    if (!response.ok) return 'other'
    const data = await response.json()
    return ['loopback-development', 'authenticated'].includes(data.mode) ? 'polygate' : 'other'
  } catch (error) {
    if (error instanceof SyntaxError) return 'other'
    return 'offline'
  }
}

async function checkWeb() {
  try {
    const response = await fetch('http://127.0.0.1:5173/', { signal: AbortSignal.timeout(1000) })
    const html = await response.text()
    return response.ok && html.includes('<title>PolyGate') ? 'polygate' : 'other'
  } catch {
    return 'offline'
  }
}

function waitForExit(child) {
  return new Promise((resolvePromise, reject) => {
    child.once('error', reject)
    child.once('exit', code => code === 0 ? resolvePromise() : reject(new Error(`Process exited with code ${code}`)))
  })
}

let api
let web
let buildDir
let stopping = false

async function stop(code = 0) {
  if (stopping) return
  stopping = true
  web?.kill()
  api?.kill()
  if (buildDir) await rm(buildDir, { recursive: true, force: true }).catch(() => {})
  process.exit(code)
}

process.on('SIGINT', () => { void stop() })
process.on('SIGTERM', () => { void stop() })

try {
  const initial = await checkAPI()
  if (initial === 'other') throw new Error('Port 8080 is already used by another service.')
  if (initial === 'offline') {
    buildDir = await mkdtemp(join(tmpdir(), 'polygate-dev-'))
    const binary = join(buildDir, process.platform === 'win32' ? 'polygate-api.exe' : 'polygate-api')
    console.log('Building the PolyGate local API...')
    const build = spawn(findGo(), ['build', '-o', binary, './cmd/api'], { cwd: rootDir, stdio: 'inherit' })
    await waitForExit(build)
    api = spawn(binary, [], {
      cwd: rootDir,
      env: { ...process.env, POLYGATE_DEV_MODE: '1' },
      stdio: 'inherit',
    })
    api.once('error', error => { console.error(error); void stop(1) })
    api.once('exit', code => { if (!stopping) { console.error(`Local API exited with code ${code}`); void stop(1) } })
    let state = 'offline'
    for (let i = 0; i < 40 && state === 'offline'; i++) {
      await new Promise(resolvePromise => setTimeout(resolvePromise, 250))
      state = await checkAPI()
    }
    if (state !== 'polygate') throw new Error('Local API did not become ready on port 8080.')
  } else {
    console.log('Using the PolyGate local API already running on port 8080.')
  }
  const frontend = await checkWeb()
  if (frontend === 'other') throw new Error('Port 5173 is already used by another service.')
  if (frontend === 'polygate') {
    console.log('Using the PolyGate console already running on http://127.0.0.1:5173/')
  } else {
    console.log('Starting the English student console on http://127.0.0.1:5173/')
    web = spawn(process.execPath, [join(webDir, 'node_modules', 'vite', 'bin', 'vite.js'), '--host', '127.0.0.1', '--strictPort'], { cwd: webDir, stdio: 'inherit' })
    web.once('error', error => { console.error(error); void stop(1) })
    web.once('exit', code => { if (!stopping) void stop(code ?? 1) })
  }
} catch (error) {
  console.error(error instanceof Error ? error.message : error)
  await stop(1)
}
