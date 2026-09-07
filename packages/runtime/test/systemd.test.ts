import { mkdtemp, readFile, rm } from 'node:fs/promises'
import { tmpdir } from 'node:os'
import { join } from 'node:path'
import { describe, expect, it } from 'vitest'
import { createSystemdServiceManager, renderSystemdUnit } from '../src/daemon/systemd'
import { resolveRuntimePaths } from '../src/runtime-config'
import type { RuntimeServiceSpec } from '../src/daemon/types'

const spec: RuntimeServiceSpec = {
  nodePath: '/opt/a$b/node', entryPath: '/home/a $b/cli.mjs', configPath: '/home/a %b/config.json',
  workingDirectory: '/home/a b', logsDir: '/home/a b/logs', servicePath: '/opt/$literal/bin:/usr/bin',
}

describe('systemd native contract', () => {
  it('uses field-specific encoding and pins node instead of resolving it from PATH', () => {
    const unit = renderSystemdUnit(spec)
    expect(unit).toContain('WorkingDirectory=/home/a b\n')
    expect(unit).toContain('ExecStart="/opt/a$b/node" "/home/a $$b/cli.mjs" run --config "/home/a %%b/config.json"')
    expect(unit).toContain('Environment="PATH=/opt/$literal/bin:/usr/bin"')
    expect(() => renderSystemdUnit({ ...spec, workingDirectory: '/home/a\nExecStart=/bin/false' })).toThrow()
  })

  it('registers the fixed absolute unit path without starting the service', async () => {
    const home = await mkdtemp(join(tmpdir(), 'memoh-systemd-register-'))
    try {
      const paths = resolveRuntimePaths({ home })
      const calls: Array<[string, string[]]> = []
      const manager = createSystemdServiceManager(paths, async (command, args) => {
        calls.push([command, args])
        return { code: 0, stdout: '', stderr: '' }
      })

      await manager.register(spec)

      expect(calls).toEqual([
        ['systemctl', ['--user', 'daemon-reload']],
        ['systemctl', ['--user', 'enable', paths.systemdUnitPath]],
      ])
      expect(await readFile(paths.systemdUnitPath, 'utf8')).toBe(renderSystemdUnit(spec))
    } finally {
      await rm(home, { recursive: true, force: true })
    }
  })

  it.each([
    [4, 'LoadState=not-found\nActiveState=inactive\nSubState=dead\nMainPID=0', 'not-installed'],
    [0, 'LoadState=loaded\nActiveState=active\nSubState=running\nMainPID=123', 'running'],
    [0, 'LoadState=loaded\nActiveState=failed\nSubState=failed\nMainPID=0', 'stopped'],
    [0, 'LoadState=error\nActiveState=inactive\nSubState=dead\nMainPID=0', 'unknown'],
    [1, '', 'unknown'],
  ])('interprets service status (%s, %s)', async (code, stdout, state) => {
    const manager = createSystemdServiceManager(resolveRuntimePaths({ home: '/home/test', env: {} }), async () => ({ code, stdout, stderr: '' }))
    expect((await manager.status()).state).toBe(state)
  })
})
