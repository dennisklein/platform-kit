// Typed client for the bench API. Types come from schema.d.ts, which
// `npm run gen` makes from api/openapi.yaml.
import createClient, { type Middleware } from 'openapi-fetch';
import type { paths } from './schema';

export const BASE = '/api/v1';

// fetch is looked up per call, so the dev mock (?mock=1) can replace it.
export const api = createClient<paths>({
  baseUrl: BASE,
  credentials: 'same-origin',
  fetch: (req) => globalThis.fetch(req),
});

/** Error body of the API (schema Error) with the HTTP status. */
export class ApiError extends Error {
  status: number;
  detail?: string;
  paths?: string[];
  constructor(status: number, body: unknown) {
    const b = (body ?? {}) as { error?: string; detail?: string; paths?: string[] };
    super(b.error || `HTTP ${status}`);
    this.status = status;
    this.detail = b.detail;
    this.paths = b.paths;
  }
}

// Clear the cached identity when the session is gone.
const authMiddleware: Middleware = {
  async onResponse({ response }) {
    if (response.status === 401) {
      window.dispatchEvent(new CustomEvent('pk:unauthorized'));
    }
    return undefined;
  },
};
api.use(authMiddleware);

/**
 * Unwrap an openapi-fetch result: return data or throw ApiError.
 * Use as `const me = await must(api.GET('/me'))`.
 */
export async function must<T>(
  p: Promise<{ data?: T; error?: unknown; response: Response }>,
): Promise<T> {
  const { data, error, response } = await p;
  if (error !== undefined || !response.ok) {
    throw new ApiError(response.status, error);
  }
  return data as T;
}

/** Message for an error from must() or fetch. */
export function errorText(e: unknown): string {
  if (e instanceof ApiError) {
    return e.detail ? `${e.message}: ${e.detail}` : e.message;
  }
  if (e instanceof Error) return e.message;
  return String(e);
}
