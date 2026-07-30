import assert from 'node:assert/strict'
import { spawnSync } from 'node:child_process'
import { chmodSync, mkdirSync, mkdtempSync, readFileSync, rmSync, writeFileSync } from 'node:fs'
import { tmpdir } from 'node:os'
import { dirname, join } from 'node:path'
import test from 'node:test'
import { fileURLToPath } from 'node:url'

import { detectPublishAuthMode } from './publish-auth.mjs'

const ROOT_DIR = dirname(dirname(fileURLToPath(import.meta.url)))

function createFakePublishCommands(prefix) {
  const tempDir = mkdtempSync(join(tmpdir(), prefix))
  const fakeBin = join(tempDir, 'bin')
  const npmLog = join(tempDir, 'npm.log')
  const pnpmLog = join(tempDir, 'pnpm.log')

  mkdirSync(fakeBin)

  const fakeNpm = join(fakeBin, 'npm')
  writeFileSync(fakeNpm, `#!/bin/sh
printf '%s\\n' "$*" >> "$PUBLISH_TEST_NPM_LOG"
[ "$1" = "view" ] && exit 1
exit 91
`)
  chmodSync(fakeNpm, 0o755)

  const fakePnpm = join(fakeBin, 'pnpm')
  writeFileSync(fakePnpm, `#!/bin/sh
printf '%s\\n' "$*" >> "$PUBLISH_TEST_PNPM_LOG"
exit 0
`)
  chmodSync(fakePnpm, 0o755)

  return {
    env: {
      ...process.env,
      ACTIONS_ID_TOKEN_REQUEST_URL: 'https://github.example.test/oidc',
      ACTIONS_ID_TOKEN_REQUEST_TOKEN: 'github-oidc-request-token',
      NODE_AUTH_TOKEN: 'XXXXX-XXXXX-XXXXX-XXXXX',
      PATH: `${fakeBin}:${process.env.PATH}`,
      PUBLISH_TEST_NPM_LOG: npmLog,
      PUBLISH_TEST_PNPM_LOG: pnpmLog,
    },
    npmLog,
    pnpmLog,
    cleanup: () => rmSync(tempDir, { recursive: true, force: true }),
  }
}

test('prefers GitHub OIDC over the setup-node token placeholder', () => {
  assert.equal(detectPublishAuthMode({
    ACTIONS_ID_TOKEN_REQUEST_URL: 'https://github.example.test/oidc',
    ACTIONS_ID_TOKEN_REQUEST_TOKEN: 'github-oidc-request-token',
    NODE_AUTH_TOKEN: 'XXXXX-XXXXX-XXXXX-XXXXX',
  }), 'oidc')
})

test('prefers GitHub OIDC when a token is also present', () => {
  assert.equal(detectPublishAuthMode({
    ACTIONS_ID_TOKEN_REQUEST_URL: 'https://github.example.test/oidc',
    ACTIONS_ID_TOKEN_REQUEST_TOKEN: 'github-oidc-request-token',
    NODE_AUTH_TOKEN: 'manual-publish-token',
  }), 'oidc')
})

test('uses token mode outside an OIDC environment', () => {
  assert.equal(detectPublishAuthMode({
    NODE_AUTH_TOKEN: 'manual-publish-token',
  }), 'token')
})

test('does not accept an incomplete OIDC environment', () => {
  assert.equal(detectPublishAuthMode({
    ACTIONS_ID_TOKEN_REQUEST_TOKEN: 'github-oidc-request-token',
  }), 'none')
})

test('publish script skips token preflight when setup-node placeholder and OIDC coexist', () => {
  const fixture = createFakePublishCommands('memoh-publish-auth-')

  try {
    const result = spawnSync(process.execPath, ['scripts/publish-packages.mjs'], {
      cwd: ROOT_DIR,
      encoding: 'utf8',
      env: {
        ...fixture.env,
        NPM_PUBLISH_SCOPE: '@memohai',
        NPM_PUBLISH_EXCLUDE: '@memohai/runtime',
      },
    })

    assert.equal(result.status, 0, result.stderr || result.stdout)
    assert.match(result.stdout, /OIDC trusted publishing: skipping token preflight/)

    const npmCalls = readFileSync(fixture.npmLog, 'utf8')
    assert.doesNotMatch(npmCalls, /^whoami$/m)
    assert.doesNotMatch(npmCalls, /^access /m)

    const pnpmCalls = readFileSync(fixture.pnpmLog, 'utf8')
    assert.match(pnpmCalls, /publish apps\/desktop --access public --no-git-checks --provenance/)
    assert.doesNotMatch(pnpmCalls, /publish packages\/runtime /)
  } finally {
    fixture.cleanup()
  }
})

test('publish script can select only the Runtime package', () => {
  const fixture = createFakePublishCommands('memoh-runtime-publish-')

  try {
    const result = spawnSync(process.execPath, ['scripts/publish-packages.mjs'], {
      cwd: ROOT_DIR,
      encoding: 'utf8',
      env: {
        ...fixture.env,
        NPM_PUBLISH_PACKAGE: '@memohai/runtime',
      },
    })

    assert.equal(result.status, 0, result.stderr || result.stdout)
    assert.match(result.stdout, /@memohai\/runtime@/)

    const npmCalls = readFileSync(fixture.npmLog, 'utf8')
    assert.match(npmCalls, /^view @memohai\/runtime@\S+ version$/m)
    assert.equal(npmCalls.trim().split('\n').length, 1)

    const pnpmCalls = readFileSync(fixture.pnpmLog, 'utf8')
    assert.match(pnpmCalls, /^publish packages\/runtime --access public --no-git-checks --provenance$/m)
    assert.equal(pnpmCalls.trim().split('\n').length, 1)
  } finally {
    fixture.cleanup()
  }
})

test('Runtime release workflow is version-gated and owns only @memohai/runtime', () => {
  const workflow = readFileSync(join(ROOT_DIR, '.github/workflows/runtime-release.yml'), 'utf8')
  const aggregateWorkflow = readFileSync(join(ROOT_DIR, '.github/workflows/release.yml'), 'utf8')
  const bumpConfig = readFileSync(join(ROOT_DIR, 'bump.config.ts'), 'utf8')

  assert.match(workflow, /paths:\n\s+- "packages\/runtime\/package\.json"/)
  assert.match(workflow, /git show "\$\{BEFORE_SHA\}:packages\/runtime\/package\.json"/)
  assert.match(workflow, /if: needs\.detect-version\.outputs\.changed == 'true'/)
  assert.match(workflow, /concurrency:\n\s+group: runtime-release\n\s+cancel-in-progress: false\n\s+queue: max/)
  assert.match(workflow, /NPM_PUBLISH_PACKAGE: "@memohai\/runtime"/)
  assert.match(aggregateWorkflow, /NPM_PUBLISH_EXCLUDE: "@memohai\/runtime"/)
  assert.doesNotMatch(aggregateWorkflow, /'packages\/runtime\/package\.json'/)
  assert.doesNotMatch(bumpConfig, /'packages\/runtime\/package\.json'/)
})
