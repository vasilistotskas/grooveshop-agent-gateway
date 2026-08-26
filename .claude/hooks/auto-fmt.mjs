/**
 * PostToolUse hook: gofmt the edited Go file, then vet its package.
 *
 * gofmt is non-negotiable formatting, so it is applied silently. `go vet` on
 * the file's own package is fast and catches the mistakes golangci-lint would
 * flag in CI; its output is reported to Claude without blocking, since a
 * package can legitimately be mid-refactor.
 *
 * Receives JSON on stdin: { tool_name, tool_input: { file_path, ... } }
 */
import { readFileSync } from 'node:fs'
import { execFileSync } from 'node:child_process'
import { dirname, relative, sep } from 'node:path'

let input = {}
try {
  input = JSON.parse(readFileSync(0, 'utf8'))
}
catch {
  process.exit(0)
}

const filePath = input.tool_input?.file_path
if (!filePath || !/\.go$/.test(filePath)) process.exit(0)

const WIN = process.platform === 'win32'
const quote = s => (WIN
  ? (/[\s&|<>^"]/.test(s) ? `"${s.replace(/"/g, '""')}"` : s)
  : (/^[A-Za-z0-9_@%+=:,./-]+$/.test(s) ? s : `'${s.replace(/'/g, `'\\''`)}'`))

/** A shell is used so a PATH-resolved `go`/`gofmt` works the same on Windows. */
function run(cmd, args, timeout = 120_000) {
  try {
    const stdout = execFileSync([cmd, ...args].map(quote).join(' '), {
      cwd: process.cwd(),
      timeout,
      stdio: 'pipe',
      encoding: 'utf8',
      shell: true,
      env: { ...process.env, NO_COLOR: '1' },
    })
    return { ok: true, output: stdout || '' }
  }
  catch (err) {
    return {
      ok: false,
      output: `${err.stdout?.toString() || ''}${err.stderr?.toString() || ''}`,
      missing: err.code === 'ENOENT' || [127, 9009].includes(err.status),
    }
  }
}

run('gofmt', ['-w', filePath])

// Vet the file's own package rather than ./... — a whole-module vet on every
// edit is far too slow for a PostToolUse hook.
let pkg = relative(process.cwd(), dirname(filePath)).split(sep).join('/')
pkg = pkg && !pkg.startsWith('..') ? `./${pkg}` : '.'

const vet = run('go', ['vet', pkg], 120_000)
if (!vet.ok && !vet.missing && vet.output.trim()) {
  process.stdout.write(JSON.stringify({
    hookSpecificOutput: {
      hookEventName: 'PostToolUse',
      additionalContext: `go vet ${pkg}:\n${vet.output.trim().split(/\r?\n/).slice(0, 20).join('\n')}`,
    },
  }))
}
