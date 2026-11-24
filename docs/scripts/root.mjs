// The docs project folder. Code from scripts/ also runs bundled (from dist/.prerender),
// where import.meta.url points elsewhere, so look for astro.config.mjs.
import { existsSync } from 'node:fs';
import { dirname, join } from 'node:path';
import { fileURLToPath } from 'node:url';

function find() {
  const here = fileURLToPath(new URL('../', import.meta.url));
  for (let d of [here, process.cwd()]) {
    for (; d !== dirname(d); d = dirname(d)) {
      if (existsSync(join(d, 'astro.config.mjs')) && existsSync(join(d, 'src/content/docs'))) return d;
    }
  }
  throw new Error('docs: no astro.config.mjs above the current folder');
}

/** Absolute path of the docs folder, without a trailing slash. */
export const root = find();
