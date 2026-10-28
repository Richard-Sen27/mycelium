import react from '@vitejs/plugin-react';
import { writeFileSync } from 'node:fs';
import { resolve } from 'node:path';
import { VitePWA } from 'vite-plugin-pwa';
import { defineConfig, type Plugin } from 'vitest/config';

// The build goes straight into the Go embed directory. emptyOutDir clears
// it, so this plugin puts back the tracked placeholder file afterwards.
const outDir = resolve(__dirname, '../internal/web/dist');
const keepPlaceholder: Plugin = {
  name: 'mycel-keep-placeholder',
  apply: 'build',
  closeBundle() {
    writeFileSync(resolve(outDir, '.keep'), '');
  },
};

// three.js and react-three-fiber (added by the constellation view) must be
// one lazily loaded chunk that index.html never preloads.
const SCENE = /[\\/]node_modules[\\/](three|three-stdlib|@react-three|maath|troika-[^\\/]+|camera-controls|meshline)[\\/]/;
// Rollup pulls the unassigned dependencies of a manual chunk into it, so
// React and the small state libraries the scene shares with the shell get a
// chunk of their own; otherwise the entry would import the scene chunk.
const SHARED = /[\\/]node_modules[\\/](react|react-dom|scheduler|zustand|use-sync-external-store)[\\/]/;

export default defineConfig({
  plugins: [
    react(),
    VitePWA({
      strategies: 'injectManifest',
      srcDir: 'src',
      filename: 'sw.ts',
      injectRegister: false, // registered from code (no inline script: CSP)
      manifestFilename: 'manifest.webmanifest',
      includeAssets: ['favicon.svg', 'icons/*.png'],
      manifest: {
        id: '/',
        name: 'Mycelium',
        short_name: 'Mycelium',
        description: 'Approvals, agents and shared compute for your network.',
        start_url: '/approvals',
        scope: '/',
        display: 'standalone',
        orientation: 'any',
        background_color: '#07090b',
        theme_color: '#07090b',
        icons: [
          { src: '/icons/icon-192.png', sizes: '192x192', type: 'image/png' },
          { src: '/icons/icon-512.png', sizes: '512x512', type: 'image/png' },
          { src: '/icons/maskable-512.png', sizes: '512x512', type: 'image/png', purpose: 'maskable' },
          { src: '/favicon.svg', sizes: 'any', type: 'image/svg+xml' },
        ],
      },
      injectManifest: {
        rollupFormat: 'iife',
        globPatterns: ['**/*.{js,css,html,woff2,svg,png}'],
        // The 3D scene is large and only needed online in the network view.
        globIgnores: ['**/scene-*.js'],
      },
      devOptions: { enabled: false },
    }),
    keepPlaceholder,
  ],
  build: {
    outDir,
    emptyOutDir: true,
    assetsDir: 'assets',
    target: 'es2022',
    sourcemap: false,
    cssCodeSplit: true,
    modulePreload: { polyfill: false },
    rollupOptions: {
      output: {
        manualChunks(id) {
          if (SCENE.test(id)) return 'scene';
          if (SHARED.test(id)) return 'react';
          return undefined;
        },
      },
    },
  },
  server: {
    // `pnpm dev` against a running hub: MYCEL_DEV_HUB=https://hub.example:8443
    proxy: process.env.MYCEL_DEV_HUB
      ? Object.fromEntries(
          ['/api', '/ui', '/mycel.v1.'].map((p) => [p, { target: process.env.MYCEL_DEV_HUB, changeOrigin: false, secure: false }]),
        )
      : undefined,
  },
  test: {
    environment: 'jsdom',
    globals: true,
    setupFiles: ['src/test/setup.ts'],
    include: ['src/**/*.test.{ts,tsx}'],
    css: { modules: { classNameStrategy: 'non-scoped' } },
    // Generous limits: the suite also runs while Go builds load the machine.
    testTimeout: 20000,
    hookTimeout: 20000,
  },
});
