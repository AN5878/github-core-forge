// Shared HTTP plumbing for the entry: auth, JSON bodies, request ids, pagination.

import type { IncomingMessage, ServerResponse } from 'node:http';
import type { Forge } from './store.ts';

export interface Ctx {
  forge: Forge;
  token: string;
  reqSeq: { n: number };
}

export function readBody(req: IncomingMessage): Promise<Buffer> {
  return new Promise<Buffer>((resolve, reject) => {
    const chunks: Buffer[] = [];
    req.on('data', (chunk: Buffer) => chunks.push(chunk));
    req.on('end', () => resolve(Buffer.concat(chunks)));
    req.on('error', reject);
  });
}

export function sendJson(res: ServerResponse, status: number, body: unknown, extraHeaders?: Record<string, string>): void {
  const text = JSON.stringify(body);
  res.writeHead(status, {
    'content-type': 'application/json',
    'content-length': Buffer.byteLength(text),
    ...extraHeaders,
  });
  res.end(text);
}

export function sendText(res: ServerResponse, status: number, text: string, contentType: string): void {
  res.writeHead(status, { 'content-type': contentType, 'content-length': Buffer.byteLength(text) });
  res.end(text);
}

/** A request id for the X-GitHub-Request-Id header (deterministic, counter-based). */
export function nextRequestId(ctx: Ctx): string {
  ctx.reqSeq.n += 1;
  return `req-${ctx.reqSeq.n}`;
}

/** Parse a JSON body; returns null (and the caller answers 400) on malformed JSON. */
export function parseJsonBody(buf: Buffer): { ok: true; value: unknown } | { ok: false } {
  if (buf.length === 0) return { ok: true, value: {} };
  try {
    return { ok: true, value: JSON.parse(buf.toString('utf8')) };
  } catch {
    return { ok: false };
  }
}

export function isRecord(value: unknown): value is Record<string, unknown> {
  return value !== null && typeof value === 'object' && !Array.isArray(value);
}

/** True when the request carries the trial's bearer <redacted> */
export function checkAuth(req: IncomingMessage, ctx: Ctx): boolean {
  const header = req.headers.authorization ?? '';
  if (ctx.token === '') return header.startsWith('Bearer ') && header.length > 7;
  return header === `Bearer ${ctx.token}`;
}

export interface PageParams {
  page: number;
  perPage: number;
}

/** `page`/`per_page` from the query, clamped to sane bounds. Defaults: page 1, per_page 30. */
export function pageParams(query: URLSearchParams, defaultPerPage = 30): PageParams {
  const page = Math.max(1, Number.parseInt(query.get('page') ?? '1', 10) || 1);
  const perPageRaw = Number.parseInt(query.get('per_page') ?? '', 10);
  const perPage = Number.isFinite(perPageRaw) ? Math.min(100, Math.max(1, perPageRaw)) : defaultPerPage;
  return { page, perPage };
}

/**
 * Slice `items` to one page and build the Link header (same-origin absolute URL)
 * when another page follows.
 */
export function paginate<T>(
  items: readonly T[],
  params: PageParams,
  req: IncomingMessage,
  pathname: string,
  query: URLSearchParams,
): { page: T[]; link: string | null } {
  const start = (params.page - 1) * params.perPage;
  const page = items.slice(start, start + params.perPage);
  let link: string | null = null;
  if (start + params.perPage < items.length) {
    const host = req.headers.host ?? '127.0.0.1';
    const next = new URLSearchParams(query);
    next.set('page', String(params.page + 1));
    next.set('per_page', String(params.perPage));
    link = `<http://${host}${pathname}?${next.toString()}>; rel="next"`;
  }
  return { page, link };
}

/** Split a pathname into decoded segments (empty segments dropped). */
export function pathSegments(pathname: string): string[] {
  return pathname
    .split('/')
    .filter((segment) => segment !== '')
    .map((segment) => {
      try {
        return decodeURIComponent(segment);
      } catch {
        return segment;
      }
    });
}
