import { chmod, mkdir, mkdtemp, readFile, rm, writeFile } from 'node:fs/promises'
import { tmpdir } from 'node:os'
import { join } from 'node:path'
import { afterEach, describe, expect, it, vi } from 'vitest'
import { runCLI, type CLIContext } from '../src/cli-main'
import { readInstallManifest, readRuntimeEnrollment, resolveRuntimePaths, writeRuntimeEnrollment, normalizeRuntimeEnrollment } from '../src/runtime-config'

const keyA = `mrk_${'a'.repeat(64)}`
const keyB = `mrk_${'b'.repeat(64)}`
const flagsA = ['--server', 'https://one.example', '--key', keyA]
const flagsB = ['--server', 'https://two.example', '--key', keyB]
const roots: string[] = []
afterEach(async () => { await Promise.all(roots.splice(0).map(path => rm(path, { recursive: true, force: true }))) })

describe('runtime CLI command ownership', () => {
  it('saves enrollment explicitly and keeps temporary runs read-only', async () => {
    const f = await fixture()
    await runCLI(flagsA, f.context)
    await expect(readFile(f.paths.configPath)).rejects.toMatchObject({ code: 'ENOENT' })
    await runCLI(['enroll', ...flagsA], f.context)
    expect(f.context.runner).not.toHaveBeenCalled()
    await runCLI(['run', ...flagsB], f.context)
    await runCLI([], f.context)
    expect(f.createSession).toHaveBeenLastCalledWith(expect.objectContaining({ key: keyA }), expect.any(Object))
    expect((await readRuntimeEnrollment(f.paths.configPath)).key).toBe(keyA)
  })

  it('requires explicit replacement for imported credentials and repairs corrupt enrollment', async () => {
    const f = await fixture()
    await runCLI(['enroll', ...flagsA], f.context)
    const other = join(f.root, 'other.json')
    await writeRuntimeEnrollment(other, normalizeRuntimeEnrollment({ serverUrl: 'https://two.example', key: keyB }))
    await expect(runCLI(['enroll', '--config', other], f.context)).rejects.toThrow('--replace')
    await runCLI(['enroll', '--config', other, '--replace'], f.context)
    expect((await readRuntimeEnrollment(f.paths.configPath)).key).toBe(keyB)
    await writeFile(f.paths.configPath, '{broken')
    await runCLI(['enroll', ...flagsA, '--replace'], f.context)
    expect((await readRuntimeEnrollment(f.paths.configPath)).key).toBe(keyA)
    expect(f.context.runner).not.toHaveBeenCalled()
  })

  it('treats explicit config as read-only input and ignores inherited credentials', async () => {
    const f = await fixture()
    const input = join(f.root, 'input.json')
    await writeRuntimeEnrollment(input, normalizeRuntimeEnrollment({ serverUrl: 'https://one.example', key: keyA }))
    await runCLI(['run', '--config', input], { ...f.context, env: { MEMOH_RUNTIME_SERVER: 'https://two.example', MEMOH_RUNTIME_KEY: keyB } })
    expect(f.createSession).toHaveBeenLastCalledWith(expect.objectContaining({ key: keyA }), expect.any(Object))
    await expect(readFile(f.paths.configPath)).rejects.toMatchObject({ code: 'ENOENT' })
  })

  it('installs without credentials and starts only after explicit enrollment and start', async () => {
    const f = await fixture()
    await runCLI(['service', 'install'], f.context)
    expect(await runCLI(['service', 'status'], f.context)).toBe(1)
    await expect(readFile(f.paths.configPath)).rejects.toMatchObject({ code: 'ENOENT' })
    await expect(runCLI(['service', 'start'], f.context)).rejects.toThrow()
    await runCLI(['enroll', ...flagsA], f.context)
    await runCLI(['service', 'start'], f.context)
    expect(await runCLI(['service', 'status'], f.context)).toBe(0)
    const previous = (await readInstallManifest(f.paths.manifestPath))!
    await runCLI(['service', 'install'], f.context)
    const next = (await readInstallManifest(f.paths.manifestPath))!
    expect(next.spec.entryPath).not.toBe(previous.spec.entryPath)
    expect(await runCLI(['service', 'status'], f.context)).toBe(1)
    expect((await readRuntimeEnrollment(f.paths.configPath)).key).toBe(keyA)
    expect(await readFile(f.paths.systemdUnitPath, 'utf8')).not.toContain(keyA)
    expect(await readFile(f.paths.manifestPath, 'utf8')).not.toContain(keyA)
  })

  it('validates restart before stopping a running process', async () => {
    const f = await fixture()
    await runCLI(['enroll', ...flagsA], f.context)
    await runCLI(['service', 'install'], f.context)
    await runCLI(['service', 'start'], f.context)
    const pid = f.pid()
    await writeFile(f.paths.configPath, '{broken')
    await expect(runCLI(['service', 'restart'], f.context)).rejects.toThrow()
    expect(await runCLI(['service', 'status'], f.context)).toBe(0)
    expect(f.pid()).toBe(pid)
  })

  it.each(['http://remote.example', 'http://localhost:8080'])('rejects unsafe transport before saving or stopping (%s)', async (server) => {
    const f = await fixture()
    await expect(runCLI(['enroll', '--server', server, '--key', keyA], f.context)).rejects.toThrow('require wss')
    await runCLI(['enroll', ...flagsA], f.context)
    await runCLI(['service', 'install'], f.context)
    await runCLI(['service', 'start'], f.context)
    const saved = JSON.parse(await readFile(f.paths.configPath, 'utf8'))
    await writeFile(f.paths.configPath, JSON.stringify({ ...saved, serverUrl: server }))
    await expect(runCLI(['service', 'restart'], f.context)).rejects.toThrow('require wss')
    expect(await runCLI(['service', 'status'], f.context)).toBe(0)
  })

  it.runIf(process.platform !== 'win32')('does not stop a running process when its installed program loses execution permission', async () => {
    const f = await fixture()
    await runCLI(['enroll', ...flagsA], f.context)
    await runCLI(['service', 'install'], f.context)
    await runCLI(['service', 'start'], f.context)
    const installation = (await readInstallManifest(f.paths.manifestPath))!
    await chmod(installation.spec.entryPath, 0o600)
    await expect(runCLI(['service', 'restart'], f.context)).rejects.toThrow()
    expect(await runCLI(['service', 'status'], f.context)).toBe(0)
  })

  it('stops and uninstalls despite corrupt records; purge retains enrollment', async () => {
    const f = await fixture()
    await runCLI(['enroll', ...flagsA], f.context)
    await runCLI(['service', 'install'], f.context)
    await runCLI(['service', 'start'], f.context)
    await writeFile(f.paths.manifestPath, '{broken')
    await writeFile(f.paths.configPath, '{broken')
    const pid = f.pid()
    await runCLI(['service', 'stop'], f.context)
    await runCLI(['service', 'uninstall', '--purge'], f.context)
    expect(f.pid()).toBe(pid)
    expect(await readFile(f.paths.configPath, 'utf8')).toBe('{broken')
    await expect(readFile(f.paths.manifestPath)).rejects.toMatchObject({ code: 'ENOENT' })
    await runCLI(['service', 'install'], f.context)
    expect((await readInstallManifest(f.paths.manifestPath))?.state).toBe('installed')
  })

  it('refuses incomplete installation and allows retry', async () => {
    const f = await fixture()
    const runner = f.context.runner!
    f.context.runner = async (command, args, options) => args.includes('enable')
      ? { code: 1, stdout: '', stderr: 'registration failed' } : runner(command, args, options)
    await expect(runCLI(['service', 'install'], f.context)).rejects.toThrow('registration failed')
    expect((await readInstallManifest(f.paths.manifestPath))?.state).toBe('prepared')
    await expect(runCLI(['service', 'start'], f.context)).rejects.toThrow('incomplete')
    f.context.runner = runner
    await runCLI(['service', 'install'], f.context)
    expect((await readInstallManifest(f.paths.manifestPath))?.state).toBe('installed')
  })

  it('keeps managed paths fixed and rejects mixed command responsibilities', async () => {
    const f = await fixture()
    await expect(runCLI(['service', 'install', ...flagsA], f.context)).rejects.toThrow()
    await expect(runCLI(['enroll', ...flagsA], { ...f.context, env: { MEMOH_RUNTIME_HOME: join(f.root, 'other') } })).rejects.toThrow('not supported')
    await runCLI(['service', 'install'], { ...f.context, env: { XDG_CONFIG_HOME: join(f.root, 'other') } })
    expect((await readInstallManifest(f.paths.manifestPath))?.spec.configPath).toBe(f.paths.configPath)
  })
})

async function fixture() {
  const root = await mkdtemp(join(tmpdir(), 'memoh-cli-'))
  roots.push(root)
  const source = join(root, 'package')
  await mkdir(source)
  await writeFile(join(source, 'cli.mjs'), '#!/usr/bin/env node\n')
  await writeFile(join(source, 'bridge.proto'), 'syntax = "proto3";')
  let installed = false
  let running = false
  let pid = 100
  const createSession = vi.fn(() => ({ start: async () => {}, stop: () => {} }))
  const context: Partial<CLIContext> = {
    platform: 'linux', home: root, env: { PATH: '/usr/bin:/bin' }, nodePath: process.execPath,
    entryPath: join(source, 'cli.mjs'), protoPath: join(source, 'bridge.proto'), createSession,
    pollIntervalMs: 0, timeoutMs: 100, stdout: () => {}, stderr: () => {},
    runner: vi.fn(async (command, args) => {
      if (command === 'systemctl') {
        if (args.includes('enable')) installed = true
        if (args.includes('start') || args.includes('restart')) { running = true; pid++ }
        if (args.includes('stop')) running = false
        if (args.includes('disable')) { installed = false; running = false }
        if (args.includes('show')) return { code: 0, stderr: '', stdout: `LoadState=${installed ? 'loaded' : 'not-found'}\nActiveState=${running ? 'active' : 'inactive'}\nSubState=${running ? 'running' : 'dead'}\nMainPID=${running ? pid : 0}` }
      }
      return { code: 0, stdout: '', stderr: '' }
    }),
  }
  return { root, context, paths: resolveRuntimePaths({ home: root, env: context.env }), createSession, pid: () => pid }
}
