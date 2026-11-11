import react from '@vitejs/plugin-react';
import { defineConfig } from 'vitest/config';

// The build goes straight into the Go package that embeds it. File names are
// fixed (no content hashes) so the committed output only changes when the
// sources do.
export default defineConfig({
  base: './',
  plugins: [react()],
  build: {
    outDir: '../internal/admin/ui/dist',
    emptyOutDir: true,
    sourcemap: false,
    cssCodeSplit: false,
    rollupOptions: {
      output: {
        entryFileNames: 'assets/app.js',
        chunkFileNames: 'assets/app.js',
        assetFileNames: (info) =>
          info.names.some((n) => n.endsWith('.css')) ? 'assets/app.css' : 'assets/[name][extname]',
        codeSplitting: false,
      },
    },
  },
  server: {
    proxy: {
      '/api': 'http://127.0.0.1:9090',
    },
  },
  test: {
    environment: 'jsdom',
    include: ['src/**/*.test.{ts,tsx}'],
    setupFiles: ['src/test-setup.ts'],
  },
});
