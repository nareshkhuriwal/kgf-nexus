// Copyright 2026 Khuriwal Group
// SPDX-License-Identifier: Apache-2.0

import { defineConfig, loadEnv } from 'vite';
import react from '@vitejs/plugin-react';

// The UI is served as static files by nginx in Compose, and the API lives on a
// different port. In dev we proxy /v1 so the browser sees a single origin and
// no CORS configuration is needed on the gateway; in production the nginx
// config does the same job. Keeping both paths identical means the app never
// needs to know its own deployment topology.
export default defineConfig(({ mode }) => {
  // loadEnv rather than process.env: it keeps the config free of Node globals
  // (no @types/node needed just to read one variable) and reads .env files the
  // same way the app does.
  const env = loadEnv(mode, '.', 'NEXUS_');
  return {
    plugins: [react()],
    server: {
      port: 5173,
      proxy: {
        '/v1': {
          target: env.NEXUS_API_URL || 'http://localhost:8080',
          changeOrigin: true,
        },
      },
    },
    build: { outDir: 'dist', sourcemap: true },
  };
});
