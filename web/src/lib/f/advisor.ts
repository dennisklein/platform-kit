// Advisor helpers: what a recipe reads (for the "working" card), the answer
// as Markdown (Copy), and the pacing of the reveal.

/** What each recipe tool reads, as a line of the working card. */
const READS: Record<string, string> = {
  impact: 'Reading the rendered impact',
  policy: 'Reading .pk/policy.yaml',
  lab: 'Reading the lab runs',
  history: 'Searching the merge history',
  mr: 'Reading the MR discussions',
  files: 'Reading the changed files',
  search: 'Searching the knowledge base',
};

/** Tools of the recipes the UI runs without the recipe list at hand. */
export const RECIPE_TOOLS: Record<string, string[]> = {
  risks: ['impact', 'policy', 'lab', 'history', 'mr'],
  'merge-order': ['impact', 'policy', 'lab', 'mr'],
  'lab-gaps': ['lab', 'impact', 'policy'],
  'explain-diff': ['impact'],
  'check-commit': ['files', 'impact', 'policy'],
  'commit-message': ['files', 'impact'],
};

/** Titles of the recipes, before the recipe list has loaded. */
export const RECIPE_TITLES: Record<string, string> = {
  risks: 'Risks of this change',
  'merge-order': 'Merge order with open changes',
  'lab-gaps': 'What does the lab not test?',
  'explain-diff': 'Explain the rendered diff',
  'check-commit': 'Check this commit',
  'commit-message': 'Suggest a commit message',
};

/** Who suggested an edit: "chat" → "Suggested by chat", "advisor:risks" → "Suggested by advisor · risks". */
export function suggestedBy(by: string | undefined): string {
  if (!by) return 'Suggested edit';
  const [who, recipe] = by.split(':', 2);
  if (who === 'advisor') return recipe ? `Suggested by advisor · ${recipe.replace(/-/g, ' ')}` : 'Suggested by advisor';
  return `Suggested by ${by}`;
}

/** The lines of the working card: one per tool, then the writing step. */
export function readingSteps(tools: string[] | undefined, writer = 'Writing the answer'): string[] {
  const seen = new Set<string>();
  const out: string[] = [];
  for (const t of tools ?? []) {
    const line = READS[t] ?? `Reading ${t}`;
    if (!seen.has(line)) {
      seen.add(line);
      out.push(line);
    }
  }
  out.push(writer);
  return out;
}

/** Time per step of the working card (ms). */
export const STEP_MS = 280;

/**
 * How long the working card stays at least, so the audience can follow
 * what the advisor reads: one step per tool, at most 1.6 s.
 */
export function minWorkingMs(steps: number, reduced = false): number {
  if (reduced) return 0;
  return Math.min(1600, Math.max(0, steps - 1) * STEP_MS + 200);
}

/** Whether the user asked for less motion. */
export function reducedMotion(): boolean {
  try {
    return window.matchMedia('(prefers-reduced-motion: reduce)').matches;
  } catch {
    return false;
  }
}

export interface PointLike {
  title?: string;
  text: string;
  severity: string;
  sources: { label: string }[];
}

/** An advisor answer as Markdown, for Copy. */
export function resultMarkdown(r: { title: string; points: PointLike[]; note?: string; changeId?: string }): string {
  const lines = [`### ${r.title}${r.changeId ? ` (${r.changeId})` : ''}`, ''];
  r.points.forEach((p, i) => {
    const lead = p.title ? `**${p.title.replace(/\.?$/, '.')}** ` : '';
    const src = p.sources.length ? ` (${p.sources.map((s) => s.label).join(', ')})` : '';
    lines.push(`${i + 1}. [${p.severity}] ${lead}${p.text}${src}`);
  });
  if (r.note) lines.push('', `_${r.note}_`);
  return lines.join('\n');
}

/**
 * The text typed so far after ms milliseconds: about 250 characters a
 * second, done after at most maxMs.
 */
export function typedText(text: string, ms: number, maxMs = 1200): string {
  if (ms <= 0) return '';
  const perMs = Math.max(0.25, text.length / maxMs);
  return text.slice(0, Math.min(text.length, Math.ceil(ms * perMs)));
}
