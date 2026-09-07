import { chmod, mkdir, mkdtemp, readFile, readdir, rm, writeFile } from 'node:fs/promises'
import { tmpdir } from 'node:os'
import { dirname, join } from 'node:path'
import { afterEach, describe, expect, it, vi } from 'vitest'
import * as runtimeConfig from '../src/runtime-config'
import { installRuntime, withServiceLock, type InstallationOptions } from '../src/daemon/installation'
import type { RuntimeServiceManager, RuntimeServiceState } from '../src/daemon/types'
import { readInstallManifest, resolveRuntimePaths, writeFileAtomic } from '../src/runtime-config'

const roots: string[] = []
afterEach(async () => {
  vi.restoreAllMocks()
  await Promise.all(roots.splice(0).map(path => rm(path, { recursive: true, force: true })))
})

describe('runtime service installation', () => {
  it.each([undefined, 'invalid enrollment'])('installs without reading or changing enrollment (%s)', async (enrollment) => {
    const f = await fixture()
    if (enrollment !== undefined) await writeFileAtomic(f.options.paths.configPath, enrollment, 0o600)
    await f.install()
    const record = (await readInstallManifest(f.options.paths.manifestPath))!
    expect(record.state).toBe('installed')
    expect(record.spec.configPath).toBe(f.options.paths.configPath)
    await expectProgram(record.spec.entryPath)
    expect(f.state()).toBe('stopped')
    expect(f.options.manager.start).not.toHaveBeenCalled()
    if (enrollment === undefined) {
      await expect(readFile(f.options.paths.configPath)).rejects.toMatchObject({ code: 'ENOENT' })
    } else {
      expect(await readFile(f.options.paths.configPath, 'utf8')).toBe(enrollment)
    }
  })

  it.each(['staging', 'validation'])('preserves the old running service when %s fails', async (phase) => {
    const f = await fixture()
    await f.install()
    await f.options.manager.start()
    const old = await readInstallManifest(f.options.paths.manifestPath)
    vi.mocked(f.options.manager.start).mockClear()
    if (phase === 'staging') await rm(f.options.sources.protoPath)
    else vi.mocked(f.options.manager.validate!).mockRejectedValueOnce(new Error('invalid service definition'))
    await expect(f.install()).rejects.toThrow()
    expect(await readInstallManifest(f.options.paths.manifestPath)).toEqual(old)
    await expectProgram(old!.spec.entryPath)
    expect(await readdir(f.options.paths.versionsDir)).toHaveLength(1)
    expect(f.state()).toBe('running')
    expect(f.options.manager.stop).not.toHaveBeenCalled()
    expect(f.options.manager.start).not.toHaveBeenCalled()
    expect(f.options.manager.register).toHaveBeenCalledTimes(1)
  })

  it('leaves a stopped service and prepared record on registration failure, then supports retry', async () => {
    const f = await fixture()
    await f.install()
    await f.options.manager.start()
    const old = (await readInstallManifest(f.options.paths.manifestPath))!
    vi.mocked(f.options.manager.start).mockClear()
    vi.mocked(f.options.manager.register).mockRejectedValueOnce(new Error('registration failed'))
    await expect(f.install()).rejects.toThrow('registration failed')
    const prepared = (await readInstallManifest(f.options.paths.manifestPath))!
    expect(prepared.state).toBe('prepared')
    expect(prepared.spec.entryPath).not.toBe(old.spec.entryPath)
    await expectProgram(prepared.spec.entryPath)
    await expectProgram(old.spec.entryPath)
    expect(f.state()).toBe('stopped')
    expect(f.options.manager.start).not.toHaveBeenCalled()
    await f.install()
    const installed = (await readInstallManifest(f.options.paths.manifestPath))!
    expect(installed.state).toBe('installed')
    expect(installed.spec.entryPath).not.toBe(prepared.spec.entryPath)
    expect(f.state()).toBe('stopped')
    expect(f.options.manager.start).not.toHaveBeenCalled()
  })

  it.each(['prepared', 'installed'] as const)('retains referenced programs if the %s record write fails after rename', async (phase) => {
    const f = await fixture()
    const writeManifest = runtimeConfig.writeInstallManifest
    vi.spyOn(runtimeConfig, 'writeInstallManifest').mockImplementation(async (path, record) => {
      await writeManifest(path, record)
      if (record.state === phase) throw new Error('directory fsync failed after rename')
    })
    await expect(f.install()).rejects.toThrow('directory fsync failed after rename')
    const record = (await readInstallManifest(f.options.paths.manifestPath))!
    expect(record.state).toBe(phase)
    await expectProgram(record.spec.entryPath)
    expect(await readdir(f.options.paths.versionsDir)).toHaveLength(1)
    expect(f.options.manager.register).toHaveBeenCalledTimes(phase === 'prepared' ? 0 : 1)
    expect(f.options.manager.start).not.toHaveBeenCalled()
  })

  it('does not publish a replacement when stopping the old process fails', async () => {
    const f = await fixture()
    await f.install()
    await f.options.manager.start()
    const old = await readInstallManifest(f.options.paths.manifestPath)
    vi.mocked(f.options.manager.stop).mockRejectedValueOnce(new Error('stop failed'))
    await expect(f.install()).rejects.toThrow('stop failed')
    expect(await readInstallManifest(f.options.paths.manifestPath)).toEqual(old)
    expect(await readdir(f.options.paths.versionsDir)).toHaveLength(1)
    expect(f.options.manager.register).toHaveBeenCalledTimes(1)
    expect(f.state()).toBe('running')
  })

  it('refuses to change a service whose state cannot be determined', async () => {
    const f = await fixture()
    vi.mocked(f.options.manager.status).mockResolvedValueOnce({ backend: 'test', state: 'unknown' })
    await expect(f.install()).rejects.toThrow('could not determine')
    expect(f.options.manager.register).not.toHaveBeenCalled()
    expect(f.options.manager.stop).not.toHaveBeenCalled()
  })

  it('excludes concurrent operations and releases the lock after failure', async () => {
    const f = await fixture()
    await expect(withServiceLock(f.options.paths, async () => {
      await expect(withServiceLock(f.options.paths, async () => {})).rejects.toThrow('operation is in progress')
      throw new Error('operation failed')
    })).rejects.toThrow('operation failed')
    await expect(withServiceLock(f.options.paths, async () => 'retry')).resolves.toBe('retry')
  })

  it.runIf(process.platform !== 'win32')('rejects an unsafe ancestor before any service mutation', async () => {
    const f = await fixture()
    const parent = join(f.root, '.memoh')
    await mkdir(parent)
    await chmod(parent, 0o777)
    await expect(f.install()).rejects.toThrow('not writable')
    expect(f.options.manager.register).not.toHaveBeenCalled()
    expect(f.options.manager.stop).not.toHaveBeenCalled()
  })
})

async function expectProgram(entryPath: string) {
  expect(await readFile(entryPath, 'utf8')).not.toBe('')
  expect(await readFile(join(dirname(entryPath), 'cli.mjs'), 'utf8')).toBe('executable')
  expect(await readFile(join(dirname(entryPath), 'bridge.proto'), 'utf8')).toBe('proto')
}

async function fixture() {
  const root = await mkdtemp(join(tmpdir(), 'memoh-install-'))
  roots.push(root)
  const entryPath = join(root, 'package/cli.mjs')
  const protoPath = join(root, 'package/bridge.proto')
  await mkdir(dirname(entryPath))
  await writeFile(entryPath, 'executable')
  await writeFile(protoPath, 'proto')
  let state: RuntimeServiceState = 'not-installed'
  const manager: RuntimeServiceManager = {
    backend: 'test',
    validate: vi.fn(async () => {}),
    register: vi.fn(async () => { state = 'stopped' }),
    start: vi.fn(async () => { state = 'running' }),
    stop: vi.fn(async () => { state = 'stopped' }),
    uninstall: vi.fn(async () => { state = 'not-installed' }),
    status: vi.fn(async () => ({ backend: 'test', state })),
  }
  const options: InstallationOptions = {
    paths: resolveRuntimePaths({ home: root }), manager, platform: process.platform,
    nodePath: process.execPath, sources: { entryPath, protoPath }, pollIntervalMs: 0, timeoutMs: 20,
  }
  return { root, options, state: () => state,
    install: () => withServiceLock(options.paths, () => installRuntime(options)),
  }
}
