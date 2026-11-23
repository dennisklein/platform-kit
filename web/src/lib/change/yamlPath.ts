// Where the cursor is in a block-style YAML document, by indentation. Works
// on half-typed text, which a YAML parser rejects. Used for schema
// completion and hover in the editor.
import type { Path } from './yamlEdit';

export interface YamlContext {
  /** Path of the map that holds the key at the cursor. */
  parent: Path;
  /** Key on the cursor line, when the line has "key:". */
  key?: string;
  /** The cursor is after "key:" (value position). */
  inValue: boolean;
  /** Text typed so far for the key or value. */
  prefix: string;
  /** Offset in the document where prefix starts. */
  from: number;
}

interface Frame {
  col: number;
  seg: string | number;
  item: boolean;
}

const KEY = /^([A-Za-z0-9_./-]+|"[^"]*"|'[^']*'):(\s|$)/;

function unquote(k: string): string {
  return k.startsWith('"') || k.startsWith("'") ? k.slice(1, -1) : k;
}

/** Split a line into its list markers and the rest: "  - - a: b" → cols. */
function scan(line: string): { items: number[]; col: number; rest: string } {
  let i = 0;
  while (i < line.length && line[i] === ' ') i++;
  const items: number[] = [];
  while (line[i] === '-' && (line[i + 1] === ' ' || i + 1 === line.length)) {
    items.push(i);
    i += 1;
    while (line[i] === ' ') i++;
  }
  return { items, col: i, rest: line.slice(i) };
}

/** Frames (open keys and list items) after reading lines[0..n). */
function frames(lines: string[], n: number): Frame[] {
  const stack: Frame[] = [];
  const counter = new Map<number, number>();
  const popTo = (col: number) => {
    while (stack.length && stack[stack.length - 1].col >= col) {
      const f = stack.pop()!;
      if (!f.item) continue;
      counter.set(f.col, f.seg as number);
    }
    for (const c of [...counter.keys()]) if (c > col) counter.delete(c);
  };
  for (let i = 0; i < n; i++) {
    const raw = lines[i];
    const t = raw.trimStart();
    if (t === '' || t.startsWith('#') || t === '---' || t === '...') continue;
    const s = scan(raw);
    for (const c of s.items) {
      // A new item at column c: the previous item at c (if any) gives the index.
      const prev = stack.find((f) => f.item && f.col === c);
      popTo(c);
      const idx = prev ? (prev.seg as number) + 1 : counter.has(c) ? counter.get(c)! + 1 : 0;
      counter.delete(c);
      stack.push({ col: c, seg: idx, item: true });
    }
    const m = KEY.exec(s.rest);
    if (m) {
      if (!s.items.length) popTo(s.col);
      stack.push({ col: s.col, seg: unquote(m[1]), item: false });
    }
  }
  return stack;
}

function pathOf(stack: Frame[]): Path {
  return stack.map((f) => f.seg);
}

/** Context of the cursor at offset pos. */
export function contextAt(text: string, pos: number): YamlContext {
  const before = text.slice(0, pos);
  const lineStart = before.lastIndexOf('\n') + 1;
  const lines = text.split('\n');
  const lineNo = before.slice(0, lineStart).split('\n').length - 1;
  const cur = before.slice(lineStart);
  const s = scan(cur);
  const stack = frames(lines, lineNo);
  // Close frames at or right of the cursor key column.
  const keyCol = s.col;
  const open: Frame[] = [];
  for (const f of stack) if (f.col < keyCol || (f.item && f.col < keyCol)) open.push(f);
  for (const c of s.items) {
    const prev = stack.find((f) => f.item && f.col === c);
    const idx = prev ? (prev.seg as number) + 1 : 0;
    while (open.length && open[open.length - 1].col >= c) open.pop();
    open.push({ col: c, seg: idx, item: true });
  }
  const m = KEY.exec(s.rest);
  if (m) {
    const key = unquote(m[1]);
    const after = s.rest.slice(m[0].length - (m[2] ? m[2].length : 0) + 1);
    const prefix = after.trimStart();
    return {
      parent: pathOf(open),
      key,
      inValue: true,
      prefix,
      from: pos - prefix.length,
    };
  }
  return { parent: pathOf(open), inValue: false, prefix: s.rest, from: lineStart + s.col };
}

/** Path of the key on a line (1-based), for hover. */
export function pathAtLine(text: string, line: number): Path | null {
  const lines = text.split('\n');
  if (line < 1 || line > lines.length) return null;
  const st = frames(lines, line);
  const s = scan(lines[line - 1]);
  if (!KEY.test(s.rest)) return null;
  return pathOf(st);
}
