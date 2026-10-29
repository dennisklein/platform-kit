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
