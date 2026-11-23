// Line diff (LCS) for "Compare with now" and suggestion previews.
// No imports: unit tested with node --test (web/unit).

export type DiffOp = ' ' | '+' | '-';
export interface DiffLine {
  op: DiffOp;
  text: string;
  /** 1-based line in the old text (for ' ' and '-'). */
  a?: number;
  /** 1-based line in the new text (for ' ' and '+'). */
  b?: number;
}

const split = (s: string) => {
  const lines = s.split('\n');
  if (lines.length && lines[lines.length - 1] === '') lines.pop();
  return lines;
};

/** All lines of a and b with their op, in order. */
export function diffLines(oldText: string, newText: string): DiffLine[] {
  const a = split(oldText);
  const b = split(newText);
  // Trim the common head and tail; the LCS table only covers the middle.
  let start = 0;
  while (start < a.length && start < b.length && a[start] === b[start]) start++;
  let endA = a.length;
  let endB = b.length;
  while (endA > start && endB > start && a[endA - 1] === b[endB - 1]) {
    endA--;
    endB--;
  }
  const n = endA - start;
  const m = endB - start;
  const out: DiffLine[] = [];
  for (let i = 0; i < start; i++) out.push({ op: ' ', text: a[i], a: i + 1, b: i + 1 });
  if (n * m > 4_000_000) {
    // Too large for the table: show it as a replacement.
    for (let i = start; i < endA; i++) out.push({ op: '-', text: a[i], a: i + 1 });
    for (let j = start; j < endB; j++) out.push({ op: '+', text: b[j], b: j + 1 });
  } else {
    const w = m + 1;
    const t = new Uint32Array((n + 1) * w);
    for (let i = n - 1; i >= 0; i--) {
      for (let j = m - 1; j >= 0; j--) {
        t[i * w + j] =
          a[start + i] === b[start + j] ? t[(i + 1) * w + j + 1] + 1 : Math.max(t[(i + 1) * w + j], t[i * w + j + 1]);
      }
    }
    let i = 0;
    let j = 0;
    while (i < n || j < m) {
      if (i < n && j < m && a[start + i] === b[start + j]) {
        out.push({ op: ' ', text: a[start + i], a: start + i + 1, b: start + j + 1 });
        i++;
        j++;
      } else if (i < n && (j === m || t[(i + 1) * w + j] >= t[i * w + j + 1])) {
        // Deletions first on ties, as git shows them.
        out.push({ op: '-', text: a[start + i], a: start + i + 1 });
        i++;
      } else {
        out.push({ op: '+', text: b[start + j], b: start + j + 1 });
        j++;
      }
    }
  }
  for (let i = endA, j = endB; i < a.length; i++, j++) out.push({ op: ' ', text: a[i], a: i + 1, b: j + 1 });
  return out;
}

export interface Hunk {
  header: string;
  lines: DiffLine[];
}

/** Hunks with `context` unchanged lines around each change. */
export function hunks(lines: DiffLine[], context = 3): Hunk[] {
  const changed = lines.map((l) => l.op !== ' ');
  const keep = lines.map((_, i) => {
    for (let k = Math.max(0, i - context); k <= Math.min(lines.length - 1, i + context); k++) if (changed[k]) return true;
    return false;
  });
  const out: Hunk[] = [];
  let cur: DiffLine[] | null = null;
  lines.forEach((l, i) => {
    if (!keep[i]) {
      cur = null;
      return;
    }
    if (!cur) {
      cur = [];
      out.push({ header: '', lines: cur });
    }
    cur.push(l);
  });
  for (const h of out) {
    const a = h.lines.find((l) => l.a !== undefined)?.a ?? 0;
    const b = h.lines.find((l) => l.b !== undefined)?.b ?? 0;
    const na = h.lines.filter((l) => l.op !== '+').length;
    const nb = h.lines.filter((l) => l.op !== '-').length;
    h.header = `@@ -${a},${na} +${b},${nb} @@`;
  }
  return out;
}

/** Number of added and removed lines. */
export function stat(lines: DiffLine[]): { added: number; removed: number } {
  let added = 0;
  let removed = 0;
  for (const l of lines) {
    if (l.op === '+') added++;
    else if (l.op === '-') removed++;
  }
  return { added, removed };
}

/** Lines of a unified diff text (from the API) with their op. */
export function parseUnified(text: string): { op: DiffOp | '@' | 'h'; text: string }[] {
  return split(text).map((l) => {
    if (l.startsWith('+++') || l.startsWith('---')) return { op: 'h' as const, text: l };
    if (l.startsWith('@@')) return { op: '@' as const, text: l };
    if (l.startsWith('+')) return { op: '+' as const, text: l.slice(1) };
    if (l.startsWith('-')) return { op: '-' as const, text: l.slice(1) };
    return { op: ' ' as const, text: l.startsWith(' ') ? l.slice(1) : l };
  });
}

/** A piece of a changed line; changed pieces are highlighted. */
export interface Seg {
  text: string;
  changed: boolean;
}

const isWord = (c: string | undefined) => c !== undefined && /[\p{L}\p{N}_]/u.test(c);

/**
 * The changed part of a line pair, as a common head, the changed middle and
 * a common tail. The middle grows to whole words ("47" and "48", not "7" and
 * "8"). Null when the lines share too little to be one edited line.
 */
export function wordSegs(oldLine: string, newLine: string): { a: Seg[]; b: Seg[] } | null {
  if (oldLine === newLine) return null;
  const max = Math.min(oldLine.length, newLine.length);
  let p = 0;
  while (p < max && oldLine[p] === newLine[p]) p++;
  let s = 0;
  while (s < max - p && oldLine[oldLine.length - 1 - s] === newLine[newLine.length - 1 - s]) s++;
  // Grow the middle to word boundaries on both sides.
  while (p > 0 && isWord(oldLine[p - 1]) && (isWord(oldLine[p]) || isWord(newLine[p]))) p--;
  const lastA = (k: number) => oldLine[oldLine.length - k];
  const lastB = (k: number) => newLine[newLine.length - k];
  while (s > 0 && isWord(lastA(s)) && (isWord(lastA(s + 1)) || isWord(lastB(s + 1)))) s--;
  const common = p + s;
  // Mostly different: a highlight would only add noise.
  if (common < 2 || common < 0.35 * Math.max(oldLine.length, newLine.length)) return null;
  const segs = (line: string): Seg[] =>
    [
      { text: line.slice(0, p), changed: false },
      { text: line.slice(p, line.length - s), changed: true },
      { text: line.slice(line.length - s), changed: false },
    ].filter((x) => x.text !== '');
  return { a: segs(oldLine), b: segs(newLine) };
}

/**
 * Word highlights for a diff: each run of removed lines is paired, line by
 * line, with the run of added lines that follows it. The map holds the
 * segments of each paired line by its index.
 */
export function wordHighlights(lines: { op: string; text: string }[]): Map<number, Seg[]> {
  const out = new Map<number, Seg[]>();
  let i = 0;
  while (i < lines.length) {
    if (lines[i].op !== '-') {
      i++;
      continue;
    }
    const del = i;
    while (i < lines.length && lines[i].op === '-') i++;
    const add = i;
    while (i < lines.length && lines[i].op === '+') i++;
    const pairs = Math.min(add - del, i - add);
    for (let k = 0; k < pairs; k++) {
      const w = wordSegs(lines[del + k].text, lines[add + k].text);
      if (!w) continue;
      out.set(del + k, w.a);
      out.set(add + k, w.b);
    }
  }
  return out;
}
