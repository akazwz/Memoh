import { defineConfig } from 'bumpp'

export default defineConfig({
  files: [
    'package.json',
    'packages/sdk/package.json',
    // Full Memoh releases still advance Runtime. Its package publish is owned
    // separately by runtime-release.yml when this committed version changes.
    'packages/runtime/package.json',
    'packages/icons/package.json',
    'packages/config/package.json',
    'apps/web/package.json',
    'apps/desktop/package.json',
  ],
  commit: 'release: v%s',
  tag: 'v%s',
  push: true,
  all: true,
})
