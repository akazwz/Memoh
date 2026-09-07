import { describe, expect, it } from 'vitest'
import { windowsPowerShellEnv } from '../src/windows-powershell'

describe('Windows PowerShell child environment', () => {
  it('removes inherited module paths case-insensitively without changing the parent', () => {
    const parent = { Path: 'system path', PSModulePath: 'PS7 modules', psmodulepath: 'other modules', TEMP: 'temp' }
    expect(windowsPowerShellEnv(parent)).toEqual({ Path: 'system path', TEMP: 'temp' })
    expect(parent.PSModulePath).toBe('PS7 modules')
    expect(parent.psmodulepath).toBe('other modules')
  })
})
