import { mkdir, rm } from 'node:fs/promises'
import { dirname, join } from 'node:path'
import { setTimeout as sleep } from 'node:timers/promises'

import { checkExecutable, readPrivateFile, errorCode } from '../secure-files'
import { ensureDirectory, writeFileAtomic, writeInstallManifest, type RuntimePaths } from '../runtime-config'
import { runtimeClientVersion } from '../version'
import { runtimeServiceSpec, stageRuntimeArtifacts, type RuntimeArtifactSources } from './index'
import type { RuntimeServiceManager, RuntimeServiceState } from './types'

export interface InstallationOptions {
  paths: RuntimePaths
  manager: RuntimeServiceManager
  platform: NodeJS.Platform
  nodePath: string
  environmentPath?: string
  sources: RuntimeArtifactSources
  pollIntervalMs?: number
  timeoutMs?: number
  secureDirectory?(path: string): Promise<void>
}

export async function withServiceLock<T>(paths: RuntimePaths, action: () => Promise<T>): Promise<T> {
  await ensureDirectory(paths.controlHome)
  const lock = join(paths.controlHome, 'operation.lock')
  try { await mkdir(lock, { mode: 0o700 }) } catch (error) {
    if (errorCode(error) === 'EEXIST') {
      throw new Error(`another runtime service operation is in progress; if its process has exited, remove ${lock} and retry`)
    }
    throw error
  }
  try {
    await writeFileAtomic(join(lock, 'owner.json'), JSON.stringify({ pid: process.pid }), 0o600)
    return await action()
  } finally { await rm(lock, { recursive: true, force: true }) }
}

export async function waitForService(
  options: Pick<InstallationOptions, 'manager' | 'pollIntervalMs' | 'timeoutMs'>,
  target: RuntimeServiceState,
): Promise<void> {
  const deadline = Date.now() + (options.timeoutMs ?? 15_000)
  let consecutive = 0
  do {
    const status = await options.manager.status()
    const matched = status.state === target || (target === 'stopped' && status.state === 'not-installed')
    consecutive = matched ? consecutive + 1 : 0
    // Require two running observations to catch immediate startup failures.
    if (consecutive >= (target === 'running' ? 2 : 1)) return
    await sleep(options.pollIntervalMs ?? 250)
  } while (Date.now() < deadline)
  throw new Error(`runtime service did not reach ${target}; inspect the service logs`)
}

// This command only installs programs and registers the stopped service.
// Publishing a prepared record before registration makes an interrupted attempt
// explicit. Retrying install replaces it; stop/uninstall never need this record.
export async function installRuntime(options: InstallationOptions): Promise<void> {
  const { paths, manager } = options
  const before = await manager.status()
  if (before.state === 'unknown') throw new Error('could not determine the current service state; installation was not changed')
  await ensureDirectory(paths.versionsDir)
  await options.secureDirectory?.(paths.versionsDir)
  const staged = await stageRuntimeArtifacts(paths, runtimeClientVersion, options.sources, options.platform, options.nodePath)
  const directory = dirname(staged.entryPath)
  let recordWriteAttempted = false
  try {
    const spec = runtimeServiceSpec({ paths, entryPath: staged.launcherPath, nodePath: options.nodePath,
      environmentPath: options.environmentPath, platform: options.platform })
    await manager.validate?.(spec)
    if (before.state !== 'not-installed') {
      await manager.stop()
      await waitForService(options, 'stopped')
    }
    // A failed fsync may still have published the new record. Retain its files
    // whenever publication was attempted, without guessing whether rename won.
    recordWriteAttempted = true
    await writeInstallManifest(paths.manifestPath, { schemaVersion: 1, state: 'prepared', spec })
    await manager.register(spec)
    await waitForService(options, 'stopped')
    await writeInstallManifest(paths.manifestPath, { schemaVersion: 1, state: 'installed', spec })
  } catch (error) {
    if (!recordWriteAttempted) await rm(directory, { recursive: true, force: true })
    throw error
  }
}

export async function validateInstalledProgram(entryPath: string, nodePath: string): Promise<void> {
  await checkExecutable(entryPath)
  await checkExecutable(join(dirname(entryPath), 'cli.mjs'))
  await checkExecutable(nodePath)
  await readPrivateFile(join(dirname(entryPath), 'bridge.proto'))
}
