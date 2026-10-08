import react from '@vitejs/plugin-react';
import { defineConfig } from 'vitest/config';

// The console is embedded in the cactusfs binary and served from its own
// origin. Every asset is bundled: nothing is loaded from a CDN (air-gap safe).
export default defineConfig({
  plugins: [react()],
  build: {
    outDir: 'dist',
    emptyOutDir: true,
    sourcemap: false,
  },
  test: {
    environment: 'jsdom',
    include: ['src/**/*.test.{ts,tsx}'],
    setupFiles: ['src/test/setup.ts'],
  },
});
