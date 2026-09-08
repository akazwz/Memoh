import { appendFile, mkdir, realpath, rm } from 'node:fs/promises'
import { homedir } from 'node:os'
import { dirname, join, resolve } from 'node:path'
import { fileURLToPath } from 'node:url'
import { parseArgs } from 'node:util'

import type { RuntimeClientConfig } from './config'
import { createRuntimeServiceManager, spawnCommand, type CommandRunner } from './daemon'
import { installRuntime, validateInstalledProgram, waitForService, withServiceLock } from './daemon/installation'
import { secureWindowsDirectory } from './daemon/windows'
import {
  normalizeRuntimeEnrollment, parseBooleanEnvironment, readInstallManifest,
  readRuntimeEnrollment, readRuntimeEnrollmentIfExists, resolveRuntimePaths,
  sameEnrollment, writeRuntimeEnrollment, type RuntimeEnrollment,
} from './runtime-config'
import { checkDirectory, checkExecutable, ensureDirectory } from './secure-files'
import { RuntimeSession, type RuntimeSessionOptions } from './session'
import { runtimeClientVersion } from './version'

interface ManagedRuntimeSession {
  start(signal?: AbortSignal): Promise<void>
  stop(): void
}

export interface CLIContext {
  platform: NodeJS.Platform
  env: NodeJS.ProcessEnv
  home: string
  nodePath: string
  entryPath: string
  protoPath: string
  uid?: number
  runner: CommandRunner
  createSession(config: RuntimeClientConfig, options: RuntimeSessionOptions): ManagedRuntimeSession
  pollIntervalMs?: number
  timeoutMs?: number
  stdout(message: string): void
  stderr(message: string): void
}

const connectionOptions = {
  server: { type: 'string' }, key: { type: 'string' }, 'team-id': { type: 'string' },
  config: { type: 'string' }, 'insecure-localhost': { type: 'boolean' },
  help: { type: 'boolean', short: 'h' },
} as const

type Values = Record<string, string | boolean | undefined>

export async function runCLI(args: string[], overrides: Partial<CLIContext> = {}): Promise<number> {
  const entryPath = overrides.entryPath ?? fileURLToPath(import.meta.url)
  const context: CLIContext = {
    platform: process.platform, env: process.env, home: homedir(), nodePath: process.execPath,
    entryPath, protoPath: join(dirname(entryPath), 'bridge.proto'), uid: process.getuid?.(),
    runner: spawnCommand, createSession: (config, options) => new RuntimeSession(config, options),
    stdout: message => console.log(message), stderr: message => console.error(message), ...overrides,
  }
  const [command, ...rest] = args
  if (['help', '--help', '-h'].includes(command)) {
    context.stdout('Usage: memoh-runtime <enroll|run|service|version>\nUse <command> --help for details.')
    return 0
  }
  if (command === 'version' || command === '--version') { context.stdout(runtimeClientVersion); return 0 }
  if (command === 'enroll') return enroll(rest, context)
  if (command === 'service') return service(rest, context)
  if (command === 'run') return run(rest, context)
  // Original foreground invocation remains supported and never saves implicitly.
  if (!command || command.startsWith('-')) return run(args, context)
  throw new Error(`unknown command: ${command}`)
}

async function run(args: string[], context: CLIContext): Promise<number> {
  const { values } = parseArgs({ args, options: { ...connectionOptions, log: { type: 'string' } }, strict: true })
  if (values.help) {
    context.stdout('Usage: memoh-runtime run [--server <url> --key <key> | --config <file>] [--log <file>]\nReads saved enrollment when no connection is supplied. Never saves or changes a service.')
    return 0
  }
  const log = createLogFile(stringValue(values.log), context.stderr)
  const controller = new AbortController()
  const stop = () => controller.abort()
  process.once('SIGINT', stop)
  process.once('SIGTERM', stop)
  try {
    const enrollment = await resolveEnrollment(values, context)
    await context.createSession({ ...enrollment, workspaceBase: context.home }, {
      onStatus: (status, error) => { const line = error ? `${status}: ${error}` : status; context.stdout(line); log.write(line) },
      warn: message => { context.stderr(message); log.write(message) },
    }).start(controller.signal)
  } catch (error) {
    log.write(`error: ${formatCLIError(error)}`)
    throw error
  } finally {
    process.off('SIGINT', stop)
    process.off('SIGTERM', stop)
    await log.flush()
  }
  return 0
}

// Task Scheduler cannot capture a task's output, so the service invocation on
// Windows asks the process to keep its own log. Logging must never take the
// runtime down, so write failures are reported once and otherwise ignored.
function createLogFile(path: string | undefined, warn: (message: string) => void): { write(line: string): void, flush(): Promise<void> } {
  let queue = Promise.resolve()
  let reported = false
  return {
    write(line) {
      if (!path) return
      queue = queue.then(async () => {
        try {
          await mkdir(dirname(path), { recursive: true, mode: 0o700 })
          await appendFile(path, `${new Date().toISOString()} ${line}\n`, { mode: 0o600 })
        } catch (error) {
          if (!reported) { reported = true; warn(`could not write log file ${path}: ${formatCLIError(error)}`) }
        }
      })
    },
    flush: () => queue,
  }
}

async function enroll(args: string[], context: CLIContext): Promise<number> {
  const { values } = parseArgs({ args, strict: true, options: { ...connectionOptions, replace: { type: 'boolean' } } })
  if (values.help) {
    context.stdout('Usage: memoh-runtime enroll [--server <url> --key <key> | --config <file>] [--replace]\nSaves the enrollment used by future runs. A running service changes only after an explicit restart.')
    return 0
  }
  assertManagedPaths(context)
  const paths = resolveRuntimePaths({ home: context.home })
  const enrollment = await resolveEnrollment(values, context)
  await ensureDirectory(paths.controlHome)
  await withServiceLock(paths, async () => {
    // --replace is also the explicit repair path for a malformed saved file.
    if (!values.replace) {
      let current: RuntimeEnrollment | undefined
      try {
        current = await readRuntimeEnrollmentIfExists(paths.configPath, context.home)
      } catch (error) {
        throw new Error(`${formatCLIError(error)}; pass --replace to overwrite it`)
      }
      if (current && !sameEnrollment(current, enrollment)) throw new Error('saved enrollment differs; pass --replace to replace it')
    }
    await writeRuntimeEnrollment(paths.configPath, enrollment)
  })
  context.stdout(`saved enrollment to ${paths.configPath}; restart a running service to apply it`)
  return 0
}

async function service(args: string[], context: CLIContext): Promise<number> {
  const [action, ...rest] = args
  if (!action || ['help', '--help', '-h'].includes(action)) {
    context.stdout('Usage: memoh-runtime service <install|start|stop|restart|status|uninstall>\nInstall registers a stopped service. Enrollment is managed separately with enroll.')
    return 0
  }
  if (!['install', 'start', 'stop', 'restart', 'status', 'uninstall'].includes(action)) throw new Error(`unknown service command: ${action}`)
  const { values } = parseArgs({ args: rest, strict: true, options: {
    help: { type: 'boolean', short: 'h' },
    ...(action === 'status' ? { json: { type: 'boolean' as const } } : {}),
    ...(action === 'uninstall' ? { purge: { type: 'boolean' as const } } : {}),
  } })
  if (values.help) {
    context.stdout(`Usage: memoh-runtime service ${action}${action === 'uninstall' ? ' [--purge]' : action === 'status' ? ' [--json]' : ''}`)
    return 0
  }
  assertManagedPaths(context)
  const paths = resolveRuntimePaths({ home: context.home })
  const manager = createRuntimeServiceManager({ platform: context.platform, paths, runner: context.runner, uid: context.uid })
  const options = { paths, manager, platform: context.platform, nodePath: context.nodePath,
    environmentPath: context.env.PATH, sources: { entryPath: context.entryPath, protoPath: context.protoPath },
    pollIntervalMs: context.pollIntervalMs, timeoutMs: context.timeoutMs,
    secureDirectory: context.platform === 'win32' ? (path: string) => secureWindowsDirectory(path, context.runner) : undefined,
  }
  if (action === 'status') {
    const status = await manager.status()
    context.stdout(values.json ? JSON.stringify(status) : `Memoh Runtime service: ${status.state} (${status.backend})${status.detail ? `: ${status.detail}` : ''}`)
    return status.state === 'running' ? 0 : 1
  }
  await ensureDirectory(paths.controlHome)
  await withServiceLock(paths, async () => {
    switch (action) {
      case 'install': {
        const nodePath = await realpath(context.nodePath)
        await checkExecutable(nodePath)
        await installRuntime({ ...options, nodePath })
        break
      }
      case 'stop':
        await manager.stop()
        await waitForService(options, 'stopped')
        break
      case 'uninstall':
        await manager.uninstall()
        await waitForService(options, 'not-installed')
        await rm(paths.manifestPath, { force: true })
        if (values.purge) {
          await checkDirectory(paths.runtimeHome)
          await rm(paths.versionsDir, { recursive: true, force: true })
          await rm(paths.logsDir, { recursive: true, force: true })
          await rm(paths.serviceDir, { recursive: true, force: true })
        }
        break
      case 'start':
      case 'restart': {
        const installation = await readInstallManifest(paths.manifestPath)
        if (installation?.state !== 'installed') throw new Error('service installation is incomplete; run service install first')
        if (installation.spec.configPath !== paths.configPath) throw new Error('service configuration is invalid; run service install again')
        await validateInstalledProgram(installation.spec.entryPath, installation.spec.nodePath)
        await readRuntimeEnrollment(paths.configPath, context.home)
        if (action === 'restart') {
          await manager.stop()
          await waitForService(options, 'stopped')
        }
        await manager.start()
        await waitForService(options, 'running')
        break
      }
    }
  })
  context.stdout(action === 'install' ? 'installed Memoh Runtime service (stopped); run service start to connect'
    : action === 'uninstall' ? 'uninstalled Memoh Runtime service; saved enrollment was retained'
      : action === 'stop' ? 'stopped Memoh Runtime service; it starts again at the next login unless you run service uninstall'
        : `${action === 'restart' ? 'restarted' : 'started'} Memoh Runtime service`)
  return 0
}

async function resolveEnrollment(values: Values, context: CLIContext): Promise<RuntimeEnrollment> {
  const explicitConfig = stringValue(values.config)
  if (explicitConfig) {
    if (['server', 'key', 'team-id', 'insecure-localhost'].some(key => values[key] !== undefined)) throw new Error('--config cannot be combined with connection flags')
    return readRuntimeEnrollment(resolve(explicitConfig), context.home)
  }
  const serverUrl = stringValue(values.server) ?? context.env.MEMOH_RUNTIME_SERVER
  const key = stringValue(values.key) ?? context.env.MEMOH_RUNTIME_KEY
  if (serverUrl || key) {
    if (!serverUrl || !key) throw new Error('--server and --key must be provided together')
    return normalizeRuntimeEnrollment({ serverUrl, key,
      teamId: stringValue(values['team-id']) ?? context.env.MEMOH_RUNTIME_TEAM_ID,
      insecureLocalhost: values['insecure-localhost'] === true || parseBooleanEnvironment(context.env.MEMOH_RUNTIME_INSECURE_LOCALHOST, 'MEMOH_RUNTIME_INSECURE_LOCALHOST') === true,
    }, context.home)
  }
  if (['team-id', 'insecure-localhost'].some(key => values[key] !== undefined)) throw new Error('connection options require --server and --key')
  const input = context.env.MEMOH_RUNTIME_CONFIG ?? resolveRuntimePaths({ home: context.home }).configPath
  return readRuntimeEnrollment(resolve(input), context.home)
}

function assertManagedPaths(context: CLIContext): void {
  if (context.env.MEMOH_RUNTIME_HOME) throw new Error('MEMOH_RUNTIME_HOME is not supported for managed enrollment or services; use the fixed user runtime directory')
}

function stringValue(value: string | boolean | undefined): string | undefined {
  return typeof value === 'string' ? value : undefined
}

export function formatCLIError(error: unknown): string {
  return error instanceof Error ? error.message : String(error)
}
