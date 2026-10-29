import { test, expect } from '@playwright/test';

// Every page renders in the shell. Phase 2 adds screenshots and flows.
const pages = [
  '/changes',
  '/files',
  '/labs',
  '/assets',
  '/kb',
  '/work',
  '/history',
  '/chat',
  '/clusters',
  '/llm-log',
  '/settings',
  '/settings/agents',
];

for (const p of pages) {
  test(`page ${p}`, async ({ page }) => {
    await page.goto('/#' + p);
    await expect(page.locator('header.topbar')).toBeVisible();
    await expect(page.getByText('Page not found')).toHaveCount(0);
  });
}
