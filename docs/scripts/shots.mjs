// Screenshots of representative pages (mise run docs:shots), to check the look.
//
// The script serves the built site (dist/, from docs:build) and takes three views of
// each page: desktop light, desktop dark and a 390 px phone (light). Output: .shots/
// or PK_SHOTS_OUT. PK_SHOTS_PAGES selects pages by a comma-separated list of paths.
//
// Playwright and its Chromium are not dependencies of the site. The script loads them
// with require(), so NODE_PATH can point at an installed copy.
import { createReadStream, existsSync, mkdirSync, statSync } from 'node:fs';
import { createServer } from 'node:http';
import { createRequire } from 'node:module';
import { extname, join } from 'node:path';
import { base } from './site.mjs';
import { root } from './root.mjs';

const require = createRequire(import.meta.url);
let playwright;
try {
  playwright = require('playwright');
} catch {
  console.error('docs:shots: Playwright is not installed. Install it, or set NODE_PATH to a folder that has it.');
  process.exit(1);
}

const dist = join(root, 'dist');
if (!existsSync(dist)) {
  console.error('docs:shots: docs/dist does not exist. Run mise run docs:build first.');
  process.exit(1);
}
const out = process.env.PK_SHOTS_OUT || join(root, '.shots');
mkdirSync(out, { recursive: true });

/** One page of each type, and the pages with the most components. */
const defaults = [
  '',
  'start/how-to-read/',
  'learn/lab-first-cluster/',
  'concepts/approval/',
  'workflows/rack-to-node/',
  'operate/day-2/upgrade-talos/',
  'operate/runbooks/host-mismatch/',
  'reference/kinds/host/',
  'reference/kinds/hostoperation/',
  'reference/controllers/hostclaim/',
  'reference/cli/approve/',
  'reference/actions/',
  'reference/phases/',
  'reference/lab/tasks/',
  'reference/security/rbac/',
  'reference/glossary/',
];
const pages = process.env.PK_SHOTS_PAGES ? process.env.PK_SHOTS_PAGES.split(',') : defaults;

const types = { '.html': 'text/html', '.css': 'text/css', '.js': 'text/javascript', '.json': 'application/json', '.svg': 'image/svg+xml', '.woff2': 'font/woff2', '.woff': 'font/woff', '.png': 'image/png', '.webp': 'image/webp', '.xml': 'application/xml' };

function serve(dir) {
  const server = createServer((req, res) => {
    let p = decodeURIComponent(new URL(req.url, 'http://x').pathname);
    if (!p.startsWith(base)) return res.writeHead(404).end();
    p = join(dir, p.slice(base.length));
    if (existsSync(p) && statSync(p).isDirectory()) p = join(p, 'index.html');
    if (!existsSync(p)) return res.writeHead(404).end();
    res.writeHead(200, { 'content-type': types[extname(p)] ?? 'application/octet-stream' });
    createReadStream(p).pipe(res);
  });
  return new Promise((ok) => server.listen(0, '127.0.0.1', () => ok(server)));
}

/** A full-page screenshot repeats fixed parts; make the header and sidebars scroll. */
const unfix = `header.header{position:absolute!important}
  .sidebar-pane{position:absolute!important;block-size:auto!important;overflow:visible!important}
  .sidebar-content{block-size:auto!important;overflow:visible!important}`;

const views = {
  light: { viewport: { width: 1440, height: 900 }, deviceScaleFactor: 1, colorScheme: 'light' },
  dark: { viewport: { width: 1440, height: 900 }, deviceScaleFactor: 1, colorScheme: 'dark' },
  phone: { viewport: { width: 390, height: 844 }, deviceScaleFactor: 1, isMobile: true, hasTouch: true, colorScheme: 'light' },
};

const server = await serve(dist);
const url = `http://127.0.0.1:${server.address().port}${base}/`;
const browser = await playwright.chromium.launch();
const problems = [];
try {
  for (const [view, opts] of Object.entries(views)) {
    const ctx = await browser.newContext(opts);
    for (const path of pages) {
      const page = await ctx.newPage();
      const res = await page.goto(url + path, { waitUntil: 'networkidle' });
      if (!res?.ok()) problems.push(`${path}: HTTP ${res?.status()}`);
      await page.addStyleTag({ content: unfix });
      await page.evaluate(() => document.fonts.ready);
      await page.waitForTimeout(300);
      // A page must not scroll sideways on a phone.
      const wide = await page.evaluate((w) => document.documentElement.scrollWidth - w, opts.viewport.width);
      if (wide > 1) problems.push(`${path} (${view}): the page is ${wide} px wider than the screen`);
      const name = (path.replace(/\/$/, '').replaceAll('/', '-') || 'landing') + `-${view}.png`;
      await page.screenshot({ path: join(out, name), fullPage: true });
      await page.close();
    }
    await ctx.close();
  }
} finally {
  await browser.close();
  server.close();
}
for (const p of problems) console.error(`docs:shots: ${p}`);
console.log(`docs:shots: ${pages.length * Object.keys(views).length} screenshots in ${out}`);
process.exit(problems.length ? 1 : 0);
