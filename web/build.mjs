// Builds the web frontend and copies it to internal/webui/dist (embedded
// into longarm-hub and longarm-tray). Run via `go generate ./internal/webui` or
// `node web/build.mjs`. Needs node + pnpm.
import { execSync } from 'node:child_process'
import { cpSync, rmSync, mkdirSync } from 'node:fs'
import { dirname, join } from 'node:path'
import { fileURLToPath } from 'node:url'

const web = dirname(fileURLToPath(import.meta.url))
const out = join(web, '..', 'internal', 'webui', 'dist')
const run = (cmd) => execSync(cmd, { cwd: web, stdio: 'inherit', shell: true })
run('pnpm install --frozen-lockfile')
run('pnpm run build')
rmSync(out, { recursive: true, force: true })
mkdirSync(out, { recursive: true })
cpSync(join(web, 'dist'), out, { recursive: true })
console.log('web UI copied to', out)
