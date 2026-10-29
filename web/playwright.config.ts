import { defineConfig, devices } from '@playwright/test';

// UI smoke test against a running bench (docker compose up, or make run).
export default defineConfig({
  testDir: './tests',
  timeout: 30_000,
  retries: 0,
  reporter: [['list']],
  use: {
    baseURL: process.env.PK_BENCH_URL ?? 'http://127.0.0.1:8080',
    screenshot: 'only-on-failure',
  },
  projects: [{ name: 'chromium', use: { ...devices['Desktop Chrome'], viewport: { width: 1440, height: 900 } } }],
});
