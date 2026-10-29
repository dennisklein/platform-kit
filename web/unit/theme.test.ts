import { test } from 'node:test';
import assert from 'node:assert/strict';
import { resolveTheme, toggledTheme } from '../src/lib/f/theme.ts';
import { ms } from '../src/lib/f/motion.ts';

test('the theme toggle always switches what is on screen', () => {
  assert.equal(resolveTheme('system', true), 'dark');
  assert.equal(resolveTheme('system', false), 'light');
  assert.equal(toggledTheme('system', false), 'dark');
  assert.equal(toggledTheme('system', true), 'light');
  assert.equal(toggledTheme('light', false), 'dark');
  assert.equal(toggledTheme('dark', false), 'light');
});

test('reduced motion turns durations off', () => {
  assert.equal(ms(200, false), 200);
  assert.equal(ms(200, true), 0);
});
