/// <reference types="vitest/config" />
import { defineConfig } from 'vitest/config'
import react from '@vitejs/plugin-react'
import { writeFileSync } from 'node:fs'

// Go's embed refuses to compile against an empty directory, and this build
// empties its output directory before writing to it — which quietly deleted the
// committed placeholder and broke "go build" in a clean checkout. Putting it
// back is part of the build rather than something to remember.
const keepTheEmbedDirectoryNonEmpty = {
  name: 'keep-embed-placeholder',
  closeBundle() {
    writeFileSync(
      '../internal/ui/dist/.gitkeep',
      "# The web UI is built into this directory by 'make ui'.\n",
    )
  },
}

// The build lands in the Go module's embed directory: shipping runner-fleet is
// copying one binary, so the UI has to be inside it.
export default defineConfig({
  plugins: [react(), keepTheEmbedDirectoryNonEmpty],
  build: { outDir: '../internal/ui/dist', emptyOutDir: true },
  server: {
    // In development the UI runs on its own port and the daemon on 8080.
    proxy: { '/api': 'http://127.0.0.1:8080' },
  },
  test: {
    environment: 'jsdom',
    globals: true,
    setupFiles: './src/test-setup.ts',
  },
})
