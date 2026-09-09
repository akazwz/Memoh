import { execFile } from 'node:child_process'
import * as fs from 'node:fs/promises'
import { mkdtemp, readdir, realpath, rm, writeFile } from 'node:fs/promises'
import { tmpdir } from 'node:os'
import { dirname, join } from 'node:path'
import { promisify } from 'node:util'

import { afterEach, describe, expect, it, vi } from 'vitest'

import { readPrivateFile, writeFileAtomic } from '../src/secure-files'
import { protectWindowsDirectory, protectWindowsFile } from '../src/windows-file-security'

vi.mock('node:fs/promises', async importOriginal => ({
  ...await importOriginal<typeof fs>(),
}))

const directories: string[] = []

afterEach(async () => {
  vi.restoreAllMocks()
  vi.unstubAllEnvs()
  await Promise.all(directories.splice(0).map(path => rm(path, { recursive: true, force: true })))
})

describe('atomic credential writes', () => {
  it.runIf(process.platform === 'win32')('reapplies private ACLs to existing files and directories', async () => {
    // Simulate Node inheriting module paths that cannot load in Windows PowerShell.
    vi.stubEnv('PSModulePath', join(tmpdir(), 'memoh-no-powershell-modules'))
    const directory = await mkdtemp(join(tmpdir(), 'memoh-acl-repeat-'))
    directories.push(directory)
    const file = join(directory, 'credential')
    await protectWindowsDirectory(directory)
    await protectWindowsDirectory(directory)
    await writeFile(file, 'test credential')
    await protectWindowsFile(file)
    await protectWindowsFile(file)
    expect(await readPrivateFile(file)).toBe('test credential')
  }, 120_000)

  it.runIf(process.platform === 'darwin')('creates the file without inherited read access before writing any bytes', async () => {
    const path = await credentialFile()
    const parent = dirname(path)
    const command = promisify(execFile)
    await command('/bin/chmod', ['+a', 'everyone allow read,execute,file_inherit,directory_inherit', parent])
    const aclLines = async (target: string) => (await command('/bin/ls', ['-lde', target])).stdout
      .split('\n').filter(line => /^\s*\d+:/.test(line))
    const parentACL = await aclLines(parent)
    expect(parentACL.length).toBeGreaterThan(0)
    const originalOpen = (await vi.importActual<typeof fs>('node:fs/promises')).open
    let privateCreationObserved = false
    vi.spyOn(fs, 'open').mockImplementation(async (...args) => {
      const handle = await originalOpen(...args)
      if (args[1] === 'wx') {
        try {
          // Inspect at the first open, before per-file ACL changes or writes.
          expect(await aclLines(String(args[0]))).toEqual([])
          expect((await handle.stat()).mode & 0o077).toBe(0)
          privateCreationObserved = true
        } catch (error) {
          await handle.close()
          throw error
        }
      }
      return handle
    })
    await writeFileAtomic(path, 'private enrollment', 0o600)
    expect(privateCreationObserved).toBe(true)
    expect(await readPrivateFile(path)).toBe('private enrollment')
    expect(await aclLines(parent)).toEqual(parentACL)
    expect(await readdir(parent)).toEqual(['credential'])
  })

  it('preserves the committed destination and removes staging after rename fails', async () => {
    const path = await credentialFile()
    const parent = dirname(path)
    const destination = join(parent, 'existing-directory')
    await fs.mkdir(destination)
    await writeFile(join(destination, 'keep'), 'old state')
    await expect(writeFileAtomic(destination, 'new state', 0o600)).rejects.toThrow()
    expect(await fs.readFile(join(destination, 'keep'), 'utf8')).toBe('old state')
    expect((await readdir(parent)).sort()).toEqual(['credential', 'existing-directory'])
  })
})

async function credentialFile(): Promise<string> {
  const directory = await mkdtemp(join(await realpath(tmpdir()), 'memoh-credential-test-'))
  directories.push(directory)
  const path = join(directory, 'credential')
  await writeFile(path, 'old credential', { mode: 0o600 })
  return path
}
