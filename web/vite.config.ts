/// <reference types="vitest/config" />
import { defineConfig } from 'vitest/config'
import react from '@vitejs/plugin-react'

// The build lands in the Go module's embed directory: shipping runner-fleet is
// copying one binary, so the UI has to be inside it.
export default defineConfig({
  plugins: [react()],
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
