import { fileURLToPath } from 'node:url';
import react from '@vitejs/plugin-react';
import type { Plugin } from 'vite';
import { defineConfig } from 'vitest/config';

/**
 * Fontsource stylesheets list a woff2 and a woff source per unicode-range
 * chunk. Every browser LensDock supports decodes woff2, so the woff fallback
 * is dropped before Vite emits assets: the CJK families would otherwise ship
 * every chunk twice.
 */
function woff2Only(): Plugin {
  return {
    name: 'lensdock-fontsource-woff2-only',
    enforce: 'pre',
    transform(code, id) {
      const path = id.split('?')[0] ?? id;
      if (!path.includes('/@fontsource/') || !path.endsWith('.css')) return null;
      return {
        code: code.replace(/,\s*url\([^)]*\.woff\)\s*format\(['"]woff['"]\)/g, ''),
        map: null,
      };
    },
  };
}

const gateway = process.env['LENSDOCK_GATEWAY'] ?? 'http://127.0.0.1:18081';

export default defineConfig({
  plugins: [react(), woff2Only()],
  resolve: {
    alias: { '@': fileURLToPath(new URL('./src', import.meta.url)) },
  },
  server: {
    host: '127.0.0.1',
    port: 5173,
    proxy: {
      '/api': {
        target: gateway,
        changeOrigin: true,
        ws: true,
        // Match the configured Gateway host and origin without relaxing its request checks.
        headers: { Origin: gateway },
      },
      '/healthz': { target: gateway, changeOrigin: true },
      '/readyz': { target: gateway, changeOrigin: true },
    },
  },
  build: {
    outDir: 'dist',
    // @novnc/novnc uses top-level await in core/util/browser.js.
    target: 'es2022',
    assetsInlineLimit: 0,
    chunkSizeWarningLimit: 1500,
    sourcemap: false,
  },
  test: {
    environment: 'node',
    include: ['src/**/*.test.ts'],
  },
});
