import { fileURLToPath, URL } from 'node:url';

import { defineConfig, loadEnv } from 'vite';
import vue from '@vitejs/plugin-vue';
import vueDevTools from 'vite-plugin-vue-devtools';

// https://vite.dev/config/
export default defineConfig(({ mode }) => {
  process.env = {
    ...process.env,
    ...loadEnv(mode, process.cwd()),
    VITE_FAVICON: mode === 'development' ? '/favicon_dev.ico' : '/favicon.ico',
  };
  return {
    // normalize to a trailing slash: import.meta.env.BASE_URL is replaced
    // with this raw value (not Vite's slash-normalized resolved base), and
    // code concatenates paths onto it (e.g. `${BASE_URL}api/v1/`)
    base: (process.env.VITE_BASE_PATH || '/').replace(/\/?$/, '/'),
    assetsDir: 'assets',
    plugins: [vue(), vueDevTools()],
    resolve: {
      alias: {
        '@': fileURLToPath(new URL('./src', import.meta.url)),
      },
    },
    // applies to npm run dev
    server: {
      proxy: {
        '/api/v1': {
          target: 'http://localhost:3000',
          changeOrigin: true,
          logLevel: 'debug',
          ws: true,
        },
        '/version': {
          target: 'http://localhost:3000',
          changeOrigin: true,
          logLevel: 'debug',
          ws: true,
        },
        '/features': {
          target: 'http://localhost:3000',
          changeOrigin: true,
          logLevel: 'debug',
          ws: true,
        },
      },
    },
    css: {
      preprocessorOptions: {
        scss: {
          // raised by Bulma 1.0.4's own Sass, not by phenix styles
          silenceDeprecations: ['if-function', 'global-builtin'],
        },
      },
    },
    // vitest: the Playwright suite in e2e/ has its own runner
    test: {
      exclude: ['e2e/**', 'node_modules/**', 'dist/**'],
    },
  };
});
