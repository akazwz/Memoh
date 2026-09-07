import { randomUUID } from 'node:crypto'
import { readFile, rm } from 'node:fs/promises'
import { join } from 'node:path'

import {
  ensureDirectory,
  writeFileAtomic,
  type RuntimePaths,
} from '../runtime-config'
import { createLaunchdServiceManager } from './launchd'
import { createSystemdServiceManager } from './systemd'
import {
  serviceExecutablePath,
  type CommandRunner,
  type RuntimeServiceManager,
  type RuntimeServiceSpec,
} from './types'
import { createWindowsTaskServiceManager } from './windows'

export * from './types'
export { renderLaunchdPlist } from './launchd'
export { renderSystemdUnit } from './systemd'
export { renderWindowsTaskXML } from './windows'

export interface RuntimeArtifactSources {
  entryPath: string
  protoPath: string
}

export interface StagedRuntimeArtifacts {
  entryPath: string
  protoPath: string
  // macOS launches a named shell wrapper that execs the pinned Node binary.
  launcherPath: string
}

export const runtimeLauncherName = 'Memoh Runtime'

export async function stageRuntimeArtifacts(
  paths: RuntimePaths,
  version: string,
  sources: RuntimeArtifactSources,
  platform: NodeJS.Platform = process.platform,
  nodePath = process.execPath,
): Promise<StagedRuntimeArtifacts> {
  const versionDirectory = join(paths.versionsDir, `${version}-${randomUUID()}`)
  await ensureDirectory(versionDirectory)
  try {
    const entryPath = join(versionDirectory, 'cli.mjs')
    const protoPath = join(versionDirectory, 'bridge.proto')
    await copyReplacing(sources.entryPath, entryPath, 0o700)
    await copyReplacing(sources.protoPath, protoPath, 0o600)
    if (platform !== 'darwin') return { entryPath, protoPath, launcherPath: entryPath }
    const launcherPath = join(versionDirectory, runtimeLauncherName)
    const quote = (value: string) => `'${value.replaceAll('\'', '\'\\\'\'')}'`
    await writeFileAtomic(launcherPath, `#!/bin/sh\nexec ${quote(nodePath)} ${quote(entryPath)} "$@"\n`, 0o700)
    return { entryPath, protoPath, launcherPath }
  } catch (error) {
    await rm(versionDirectory, { recursive: true, force: true })
    throw error
  }
}

export function createRuntimeServiceManager(options: {
  platform: NodeJS.Platform
  paths: RuntimePaths
  runner: CommandRunner
  uid?: number
}): RuntimeServiceManager {
  switch (options.platform) {
    case 'darwin': {
      if (options.uid === undefined) throw new Error('launchd service installation requires a user ID')
      return createLaunchdServiceManager(options.paths, options.runner, options.uid)
    }
    case 'linux':
      return createSystemdServiceManager(options.paths, options.runner)
    case 'win32':
      return createWindowsTaskServiceManager(options.paths, options.runner)
    default:
      throw new Error(`background service installation is not supported on ${options.platform}`)
  }
}

export function runtimeServiceSpec(options: {
  paths: RuntimePaths
  entryPath: string
  nodePath: string
  environmentPath?: string
  platform?: NodeJS.Platform
}): RuntimeServiceSpec {
  return {
    entryPath: options.entryPath,
    configPath: options.paths.configPath,
    nodePath: options.nodePath,
    logsDir: options.paths.logsDir,
    workingDirectory: options.paths.home,
    servicePath: serviceExecutablePath(options.nodePath, options.environmentPath, options.platform),
  }
}

async function copyReplacing(source: string, destination: string, mode: number): Promise<void> {
  const content = await readFile(source)
  await writeFileAtomic(destination, content, mode)
}
