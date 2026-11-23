import { defineConfig, type Plugin } from 'vite';
import { svelte } from '@sveltejs/vite-plugin-svelte';
import { writeFileSync } from 'node:fs';
import { fileURLToPath } from 'node:url';

// The bench to proxy to in dev. Start it with
// PK_ALLOWED_HOSTS=localhost:5173 so that the Host header passes.
const bench = process.env.PK_BENCH_URL ?? 'http://127.0.0.1:8080';

// Keep dist/.gitkeep so that the Go embed compiles before the first build.
function keepDist(): Plugin {
  return {
    name: 'pk-keep-dist',
    closeBundle() {
      writeFileSync(fileURLToPath(new URL('./dist/.gitkeep', import.meta.url)), '');
    },
  };
}

export default defineConfig({
  plugins: [svelte(), keepDist()],
  build: {
    outDir: 'dist',
    emptyOutDir: true,
    chunkSizeWarningLimit: 2000,
  },
  server: {
    port: 5173,
    strictPort: true,
    proxy: {
      '/api': { target: bench, changeOrigin: false, ws: false },
      '/mcp': { target: bench, changeOrigin: false },
      '/healthz': { target: bench, changeOrigin: false },
    },
  },
});
