import { access, mkdir, rm } from 'node:fs/promises'

import { writeFileAtomic, type RuntimePaths } from '../runtime-config'
import {
  requireCommand,
  type CommandRunner,
  type RuntimeServiceManager,
  type RuntimeServiceSpec,
  type RuntimeServiceStatus,
} from './types'

const launchdLabel = 'ai.memoh.runtime'

export function renderLaunchdPlist(spec: RuntimeServiceSpec): string {
  return `<?xml version="1.0" encoding="UTF-8"?>
<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">
<plist version="1.0">
<dict>
  <key>Label</key>
  <string>${xmlEscape(launchdLabel)}</string>
  <key>ProgramArguments</key>
  <array>
    <string>${xmlEscape(spec.entryPath)}</string>
    <string>run</string>
    <string>--config</string>
    <string>${xmlEscape(spec.configPath)}</string>
  </array>
  <key>EnvironmentVariables</key>
  <dict>
    <key>PATH</key>
    <string>${xmlEscape(spec.servicePath)}</string>
  </dict>
  <key>WorkingDirectory</key>
  <string>${xmlEscape(spec.workingDirectory)}</string>
  <key>RunAtLoad</key>
  <true/>
  <key>KeepAlive</key>
  <true/>
  <key>ProcessType</key>
  <string>Background</string>
  <key>ThrottleInterval</key>
  <integer>5</integer>
  <key>StandardOutPath</key>
  <string>${xmlEscape(`${spec.logsDir}/runtime.log`)}</string>
  <key>StandardErrorPath</key>
  <string>${xmlEscape(`${spec.logsDir}/runtime-error.log`)}</string>
</dict>
</plist>
`
}

export function createLaunchdServiceManager(
  paths: RuntimePaths,
  runner: CommandRunner,
  uid: number,
): RuntimeServiceManager {
  const launchctl = '/bin/launchctl'
  const domain = `gui/${uid}`
  const target = `${domain}/${launchdLabel}`
  const loaded = async () => (await runner(launchctl, ['print', target])).code === 0
  const bootstrap = async () => {
    if (await loaded()) return
    await requireCommand(runner, launchctl, ['bootstrap', domain, paths.launchdPlistPath])
  }
  return {
    backend: 'launchd-user',
    async install(spec, options = {}) {
      await mkdir(spec.logsDir, { recursive: true, mode: 0o700 })
      await writeFileAtomic(paths.launchdPlistPath, renderLaunchdPlist(spec), 0o600)
      if (await loaded()) {
        await requireCommand(runner, launchctl, ['bootout', target], { allowedExitCodes: [0, 3] })
      }
      if (options.start !== false) {
        await requireCommand(runner, launchctl, ['bootstrap', domain, paths.launchdPlistPath])
      }
    },
    async start() {
      await bootstrap()
    },
    async stop() {
      await requireCommand(runner, launchctl, ['bootout', target], { allowedExitCodes: [0, 3] })
    },
    async restart() {
      if (await loaded()) {
        await requireCommand(runner, launchctl, ['kickstart', '-k', target])
        return
      }
      await bootstrap()
    },
    async status(): Promise<RuntimeServiceStatus> {
      const result = await runner(launchctl, ['print', target])
      if (result.code === 0) {
        return {
          backend: 'launchd-user',
          state: /\bstate\s*=\s*running\b/.test(result.stdout) ? 'running' : 'stopped',
        }
      }
      try {
        await access(paths.launchdPlistPath)
        return { backend: 'launchd-user', state: 'stopped' }
      } catch {
        return { backend: 'launchd-user', state: 'not-installed' }
      }
    },
    async uninstall() {
      await requireCommand(runner, launchctl, ['bootout', target], { allowedExitCodes: [0, 3] })
      await rm(paths.launchdPlistPath, { force: true })
    },
  }
}

function xmlEscape(value: string): string {
  if (value.includes('\0')) throw new Error('launchd service value contains a null byte')
  return value
    .replaceAll('&', '&amp;')
    .replaceAll('<', '&lt;')
    .replaceAll('>', '&gt;')
    .replaceAll('"', '&quot;')
    .replaceAll('\'', '&apos;')
}
