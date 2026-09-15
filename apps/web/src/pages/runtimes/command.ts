export interface RuntimeCommandCredential {
  key?: string
  team_id?: string
}

export function buildRuntimeConnectCommand(
  serverUrl: string,
  credential: RuntimeCommandCredential | null | undefined,
): string {
  const key = credential?.key?.trim()
  if (!key) return ''

  const args = [
    'memoh-runtime',
    'enroll',
    '--server',
    serverUrl,
    '--key',
    key,
  ]
  const teamId = credential?.team_id?.trim()
  if (teamId) {
    args.push('--team-id', teamId)
  }
  if (isInsecureLocalhost(serverUrl)) {
    args.push('--insecure-localhost')
  }
  // Stop on failure so a rejected enrollment cannot start a different saved connection.
  return [
    // Earlier published versions only support foreground connections.
    'npm install -g "@memohai/runtime@>=0.20.0"',
    args.join(' '),
    'memoh-runtime service install',
    'memoh-runtime service start',
  ].join(' && ')
}

function isInsecureLocalhost(serverUrl: string): boolean {
  const url = new URL(serverUrl)
  const hostname = url.hostname.replace(/^\[|\]$/g, '')
  return url.protocol === 'http:' && ['localhost', '127.0.0.1', '::1'].includes(hostname)
}
