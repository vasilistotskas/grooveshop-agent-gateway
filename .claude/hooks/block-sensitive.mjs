/**
 * PreToolUse hook: Block edits to sensitive or generated files in
 * grooveshop-agent-gateway.
 *
 * Blocks: .env, .env.*, go.sum, settings.local.json
 * Allows: .env.example (the template, meant to be edited)
 *
 * Node rather than shell for parity with the other four repos in the
 * workspace — and because `jq` is not a given on a Windows dev machine.
 *
 * Receives JSON on stdin: { tool_name, tool_input: { file_path, ... } }
 * Exit 0 = allow, exit 2 = block (stderr shown as the reason).
 */
import { readFileSync } from 'node:fs'

let input = {}
try {
  input = JSON.parse(readFileSync(0, 'utf8'))
}
catch {
  process.exit(0)
}

const filePath = input.tool_input?.file_path || ''
if (!filePath) process.exit(0)

const normalized = filePath.split('\\').join('/')
const base = normalized.split('/').pop()

const isEnvFile = /(^|\/)\.env($|\.)/.test(normalized)
const isEnvExample = base === '.env.example'
const isGoSum = base === 'go.sum'
const isLocalSettings = base === 'settings.local.json'

const isBlocked = (isEnvFile && !isEnvExample) || isGoSum || isLocalSettings

if (isBlocked) {
  const reason = isGoSum
    ? 'Run `go mod tidy` instead.'
    : isLocalSettings
      ? 'settings.local.json is per-machine; edit settings.json instead.'
      : 'Edit .env.example instead.'
  process.stderr.write(`BLOCKED: ${filePath} should not be edited manually. ${reason}`)
  process.exit(2)
}
