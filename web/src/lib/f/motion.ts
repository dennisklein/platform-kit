// Motion settings for Svelte transitions. Svelte animates with the Web
// Animations API, which CSS media queries do not reach, so every transition
// takes its duration from here: zero when the user asks for reduced motion.

/** True when the browser asks for reduced motion. */
export function reducedMotion(): boolean {
  try {
    return typeof matchMedia === 'function' && matchMedia('(prefers-reduced-motion: reduce)').matches;
  } catch {
    return false;
  }
}

/** A duration in ms, or 0 with reduced motion. */
export function ms(n: number, reduced = reducedMotion()): number {
  return reduced ? 0 : n;
}
