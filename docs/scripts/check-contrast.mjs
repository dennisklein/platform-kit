// Checks the color contrast of the theme tokens (mise run docs:lint:contrast).
//
// Reads the tokens of src/styles/theme.css for the dark theme (:root), the light
// theme (:root[data-theme='light']) and the dark console panels in the light theme
// (explainers and the hero panel), and computes the WCAG 2.2 contrast ratio of each
// pair below. Text needs 4.5:1 (SC 1.4.3); lines and borders that carry meaning
// need 3:1 (SC 1.4.11). The rest opacity of the explainer edges comes from
// src/styles/components.css.
// Usage: node scripts/check-contrast.mjs
import { readFileSync } from 'node:fs';
import { join } from 'node:path';
import { root } from './root.mjs';

const theme = readFileSync(join(root, 'src/styles/theme.css'), 'utf8');
const components = readFileSync(join(root, 'src/styles/components.css'), 'utf8');

/** The `--name: #hex` declarations of the first rule whose selector list starts with `selector`. */
function block(css, selector) {
  const i = css.indexOf(`${selector}`);
  if (i < 0) throw new Error(`check-contrast: no rule "${selector}"`);
  const body = css.slice(css.indexOf('{', i) + 1, css.indexOf('}', i));
  return Object.fromEntries([...body.matchAll(/--([\w-]+):\s*(#[0-9a-fA-F]{6})\b/g)].map((m) => [m[1], m[2]]));
}

const rgb = (hex) => [1, 3, 5].map((i) => parseInt(hex.slice(i, i + 2), 16));
const mix = (a, b, p) => {
  const [x, y] = [rgb(a), rgb(b)];
  return `#${x.map((v, i) => Math.round(v * p + y[i] * (1 - p)).toString(16).padStart(2, '0')).join('')}`;
};
const lum = (hex) => {
  const [r, g, b] = rgb(hex).map((v) => {
    const c = v / 255;
    return c <= 0.03928 ? c / 12.92 : ((c + 0.055) / 1.055) ** 2.4;
  });
  return 0.2126 * r + 0.7152 * g + 0.0722 * b;
};
const ratio = (a, b) => {
  const [x, y] = [lum(a), lum(b)].sort((p, q) => q - p);
  return (x + 0.05) / (y + 0.05);
};

const darkCats = block(theme, ':root,\n');
const dark = { ...block(theme, ':root {'), ...darkCats };
const light = { ...dark, ...block(theme, ":root[data-theme='light'] {") };
const panel = { ...light, ...block(theme, ":root[data-theme='light'] .pk-ex,"), ...darkCats };
const rest = Number(/\.pk-ex \.edge-g \{\s*opacity:\s*([\d.]+)/.exec(components)?.[1] ?? 1);
const cats = Object.keys(dark).filter((k) => k.startsWith('c-'));

const TEXT = 4.5;
const LINE = 3;
const results = [];
const check = (where, what, fg, bg, need) => results.push({ where, what, r: ratio(fg, bg), need });

for (const [where, t] of [['dark', dark], ['light', light]]) {
  // Category links are chips: the color on an 11 % tint of itself (cli: on paper-3).
  for (const c of cats) {
    const chip = c === 'c-cli' ? t['pk-paper-3'] : mix(t[c], t['pk-paper'], 0.11);
    check(where, `--${c} link chip`, t[c], chip, TEXT);
  }
  for (const k of ['pk-ink', 'pk-ink-2', 'pk-ink-3', 'pk-link']) check(where, `--${k} on paper`, t[k], t['pk-paper'], TEXT);
  check(where, 'primary button text', t['pk-on-accent'], t['pk-accent'], TEXT);
  check(where, 'diagram line on box', t['pk-fig-line'], t['pk-fig-box'], LINE);
  check(where, 'diagram line on page', t['pk-fig-line'], t['pk-paper'], LINE);
  check(where, 'diagram small text', t['pk-fig-dim'], t['pk-fig-box'], TEXT);
}
for (const [where, t] of [['dark explainer', dark], ['light explainer', panel]]) {
  for (const c of cats) check(where, `--${c} box title`, t[c], t['pk-ex-box'], TEXT);
  check(where, 'play button', t['pk-on-accent'], t['pk-accent'], TEXT);
  check(where, `edge at rest (opacity ${rest})`, mix(t['pk-ex-line'], t['pk-ex-bg'], rest), t['pk-ex-bg'], LINE);
  check(where, 'edge on box', t['pk-ex-line'], t['pk-ex-box'], LINE);
  check(where, 'small text', t['pk-ex-dim'], t['pk-ex-box'], TEXT);
}

const bad = results.filter((x) => x.r < x.need);
for (const x of bad) console.error(`check-contrast: ${x.where}: ${x.what}: ${x.r.toFixed(2)}:1, needs ${x.need}:1`);
if (process.argv.includes('-v')) for (const x of results) console.log(`${x.where.padEnd(16)} ${x.what.padEnd(34)} ${x.r.toFixed(2)}`);
console.log(`check-contrast: ${results.length} pairs checked, ${bad.length} below the WCAG minimum`);
process.exit(bad.length ? 1 : 0);
