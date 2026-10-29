// Unified diff parsing for the diff views (source, rendered, suggestions).

export type DiffLineKind = 'add' | 'del' | 'ctx' | 'hunk' | 'meta';

export interface DiffLine {
  kind: DiffLineKind;
  text: string;
  /** Line number in the old file (del, ctx). */
  old?: number;
  /** Line number in the new file (add, ctx). */
  new?: number;
}

export interface DiffStats {
  added: number;
  deleted: number;
}

const HUNK = /^@@ -(\d+)(?:,(\d+))? \+(\d+)(?:,(\d+))? @@(.*)$/;

/** Parse one unified diff (one file) into lines with numbers. */
export function parseUnified(text: string): DiffLine[] {
  const out: DiffLine[] = [];
  let o = 0;
  let n = 0;
  const lines = text.split('\n');
  if (lines.length && lines[lines.length - 1] === '') lines.pop();
  for (const l of lines) {
    if (l.startsWith('--- ') || l.startsWith('+++ ') || l.startsWith('diff ') || l.startsWith('index ')) {
      out.push({ kind: 'meta', text: l });
      continue;
    }
    const h = HUNK.exec(l);
    if (h) {
      o = Number(h[1]);
      n = Number(h[3]);
      out.push({ kind: 'hunk', text: l });
      continue;
    }
    if (l.startsWith('\\')) continue; // "\ No newline at end of file"
    if (l.startsWith('+')) {
      out.push({ kind: 'add', text: l.slice(1), new: n++ });
    } else if (l.startsWith('-')) {
      out.push({ kind: 'del', text: l.slice(1), old: o++ });
    } else {
      out.push({ kind: 'ctx', text: l.startsWith(' ') ? l.slice(1) : l, old: o++, new: n++ });
    }
  }
  return out;
}

export function diffStats(lines: DiffLine[]): DiffStats {
  let added = 0;
  let deleted = 0;
  for (const l of lines) {
    if (l.kind === 'add') added++;
    else if (l.kind === 'del') deleted++;
  }
  return { added, deleted };
}

/**
 * A minimal line diff of two texts (LCS), as unified-style lines with full
 * context. Used when the API gives old/new content but no diff.
 */
export function lineDiff(a: string, b: string): DiffLine[] {
  const x = a.split('\n');
  const y = b.split('\n');
  const m = x.length;
  const n = y.length;
  // Too big for O(m*n): show as replace.
  if (m * n > 4_000_000) {
    return [
      ...x.map((t, i) => ({ kind: 'del' as const, text: t, old: i + 1 })),
      ...y.map((t, i) => ({ kind: 'add' as const, text: t, new: i + 1 })),
    ];
  }
  const dp: Uint32Array[] = Array.from({ length: m + 1 }, () => new Uint32Array(n + 1));
  for (let i = m - 1; i >= 0; i--) {
    for (let j = n - 1; j >= 0; j--) {
      dp[i][j] = x[i] === y[j] ? dp[i + 1][j + 1] + 1 : Math.max(dp[i + 1][j], dp[i][j + 1]);
    }
  }
  const out: DiffLine[] = [];
  let i = 0;
  let j = 0;
  while (i < m && j < n) {
    if (x[i] === y[j]) {
      out.push({ kind: 'ctx', text: x[i], old: i + 1, new: j + 1 });
      i++;
      j++;
    } else if (dp[i + 1][j] >= dp[i][j + 1]) {
      out.push({ kind: 'del', text: x[i], old: i + 1 });
      i++;
    } else {
      out.push({ kind: 'add', text: y[j], new: j + 1 });
      j++;
    }
  }
  while (i < m) out.push({ kind: 'del', text: x[i], old: ++i });
  while (j < n) out.push({ kind: 'add', text: y[j], new: ++j });
  return out;
}

/** Collapse long unchanged runs to `context` lines around changes. */
export function collapse(lines: DiffLine[], context = 3): (DiffLine | { kind: 'gap'; count: number })[] {
  const keep = new Array(lines.length).fill(false);
  lines.forEach((l, i) => {
    if (l.kind !== 'ctx') {
      for (let k = Math.max(0, i - context); k <= Math.min(lines.length - 1, i + context); k++) keep[k] = true;
    }
  });
  const out: (DiffLine | { kind: 'gap'; count: number })[] = [];
  let gap = 0;
  lines.forEach((l, i) => {
    if (keep[i]) {
      if (gap) out.push({ kind: 'gap', count: gap });
      gap = 0;
      out.push(l);
    } else gap++;
  });
  if (gap) out.push({ kind: 'gap', count: gap });
  return out;
}
