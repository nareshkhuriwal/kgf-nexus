import { defineConfig } from 'vitest/config';
import react from '@vitejs/plugin-react';

export default defineConfig({
  plugins: [react()],
  test: {
    environment: 'jsdom',
    globals: true,
    setupFiles: ['./src/test/setup.ts'],
    include: ['src/**/*.test.{ts,tsx}'],
    // CSS is processed rather than stubbed so `import css from './index.css?raw'`
    // returns the real stylesheet. The palette contrast suite asserts against the
    // CSS that actually ships; with the default stubbing it silently received an
    // empty string and every ratio assertion passed vacuously.
    css: true,
  },
});
