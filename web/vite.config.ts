import { defineConfig } from 'vite';
import solid from 'vite-plugin-solid';

// The daemon serves dist/index.html at / and everything else under /ui/, and
// the page is embedded into the binary at build time (crates/hive-webui). Two
// fixed file names rather than hashed ones: the daemon owns the cache policy
// (no-cache, so a stale app.js against a new API is not a support call) and
// the webui tests name the files.
export default defineConfig({
  plugins: [solid()],
  base: '/ui/',
  build: {
    outDir: 'dist',
    emptyOutDir: true,
    target: 'es2022',
    sourcemap: false,
    // The polyfill is an inline script, and the page runs under
    // script-src 'self'. One entry chunk needs no preloading anyway.
    modulePreload: { polyfill: false },
    cssCodeSplit: false,
    rollupOptions: {
      output: {
        entryFileNames: 'app.js',
        chunkFileNames: 'chunk-[name].js',
        assetFileNames: (info) => {
          const names = info.names ?? (info.name ? [info.name] : []);
          return names.some((n) => n.endsWith('.css')) ? 'styles.css' : '[name][extname]';
        },
      },
    },
  },
});
