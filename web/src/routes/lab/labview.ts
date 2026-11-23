// Pure helpers of the lab run page: the live console, the elapsed clock
// and the view of a run that exists only as its report (seeded runs, runs
// of another bench).
import type { LabStep } from '../../lib/api/types';

export type ConsoleLine =
  | { kind: 'head'; key: string; step: string; title: string; status: LabStep['status']; ms?: number }
  | { kind: 'out'; key: string; step: string; text: string; bad: boolean }
  | { kind: 'checks'; key: string; step: string; pass: number; fail: number }
  | { kind: 'running'; key: string; step: string; title: string; since?: string };

/** Lines of a step that the console shows (output, then failed checks are marked). */
function outputOf(s: LabStep): string[] {
  return s.output ?? [];
}

/**
 * The console of a run: every started step in order, with a heading, its
 * output and a check summary. A running step ends the console with a
 * "running" line. revealed caps the output lines shown per step (lines
 * stream in one by one after a step ends); a step without an entry shows
 * all its lines.
 */
export function consoleLines(steps: LabStep[], revealed: Record<string, number> = {}): ConsoleLine[] {
  const out: ConsoleLine[] = [];
  for (const s of [...steps].sort((a, b) => a.index - b.index)) {
    if (s.status === 'pending' || s.status === 'skipped') continue;
    const title = s.detail ? `${s.title} · ${s.detail}` : s.title;
    if (s.status === 'running') {
      out.push({ kind: 'running', key: `${s.name}:run`, step: s.name, title: s.title, since: s.startedAt });
      continue;
    }
    out.push({ kind: 'head', key: `${s.name}:head`, step: s.name, title, status: s.status, ms: s.durationMs });
    const lines = outputOf(s);
    const n = Math.min(lines.length, revealed[s.name] ?? lines.length);
    const failed = (s.checks ?? []).filter((c) => !c.ok).map((c) => c.detail ?? '').filter(Boolean);
    for (let i = 0; i < n; i++) {
      const text = lines[i];
      out.push({ kind: 'out', key: `${s.name}:${i}`, step: s.name, text, bad: failed.some((f) => text.includes(f)) });
    }
    if (n === lines.length && s.checks?.length) {
      const fail = s.checks.filter((c) => !c.ok).length;
      out.push({ kind: 'checks', key: `${s.name}:checks`, step: s.name, pass: s.checks.length - fail, fail });
    }
  }
  return out;
}

/** Elapsed time as a clock: 0:07, 1:23, 12:05. */
export function elapsed(ms: number): string {
  if (!Number.isFinite(ms) || ms < 0) ms = 0;
  const s = Math.floor(ms / 1000);
  return `${Math.floor(s / 60)}:${String(s % 60).padStart(2, '0')}`;
}

export interface ReportStep {
  title: string;
  detail: string;
  result: string;
}
export interface ReportCheck {
  ok: boolean;
  text: string;
}
export interface ParsedReport {
  result?: string;
  steps: ReportStep[];
  checks: ReportCheck[];
  failures: string[];
  /** Prose next to the checks and failures, for example why a check fails. */
  notes: string[];
}

function section(body: string, name: string): string[] {
  const lines = body.split('\n');
  const start = lines.findIndex((l) => l.trim().toLowerCase() === `## ${name}`.toLowerCase());
  if (start < 0) return [];
  const rest = lines.slice(start + 1);
  const end = rest.findIndex((l) => /^#{1,2} /.test(l));
  return end < 0 ? rest : rest.slice(0, end);
}

/** Steps, checks and failures of a lab report article (kb/labs/<id>-run-N.md). */
export function parseReport(body: string): ParsedReport {
  const m = body.match(/Result:\s*\*\*(\w+)\*\*/);
  const steps: ReportStep[] = [];
  for (const l of section(body, 'Steps')) {
    if (!l.trim().startsWith('|')) continue;
    const cells = l
      .trim()
      .replace(/^\||\|$/g, '')
      .split('|')
      .map((c) => c.trim().replace(/\\\|/g, '|'));
    if (cells.length < 3 || /^-+$/.test(cells[0]) || cells[0].toLowerCase() === 'step') continue;
    steps.push({ title: cells[0], detail: cells[1], result: cells[2].toLowerCase() });
  }
  const checks: ReportCheck[] = [];
  for (const l of section(body, 'Checks')) {
    const c = l.match(/^\s*[-*]\s+(pass|fail):\s*(.+)$/i);
    if (c) checks.push({ ok: c[1].toLowerCase() === 'pass', text: c[2].trim() });
  }
  const failures = section(body, 'Failures')
    .map((l) => l.match(/^\s*[-*]\s+(.+)$/)?.[1]?.trim())
    .filter((x): x is string => !!x);
  // Prose after the check list: the explanation of a failure.
  const notes: string[] = [];
  let para: string[] = [];
  for (const l of [...section(body, 'Checks'), '', ...section(body, 'Failures'), '']) {
    if (l.trim() && !/^\s*[-*]\s+/.test(l)) {
      para.push(l.trim());
      continue;
    }
    const p = para.join(' ');
    if (p && p !== 'No checks ran.') notes.push(p);
    para = [];
  }
  return { result: m?.[1]?.toLowerCase(), steps, checks, failures, notes };
}

/** Status of a report step for the track. */
export function reportStatus(result: string): 'done' | 'fail' | 'skipped' | 'todo' {
  if (result === 'pass') return 'done';
  if (result === 'fail') return 'fail';
  if (result === 'skipped') return 'skipped';
  return 'todo';
}

/** The change id of a run id (c-k7m2qz/run-1 → c-k7m2qz). */
export function changeOfRun(run: string): string | undefined {
  const i = run.indexOf('/');
  return i > 0 ? run.slice(0, i) : undefined;
}
