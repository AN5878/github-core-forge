// The server app: one http server, four surfaces. GET /healthz takes no token;
// every other route requires the trial's bearer <redacted> (MAKEFUN_SHOP_TOKEN).
// Unknown routes are JSON 404s; a handler that throws answers 500 and the
// process stays up.

import { createServer, type IncomingMessage, type ServerResponse } from 'node:http';
import { handleGit } from './src/githttp.ts';
import { handleGraphQL } from './src/graphql.ts';
import { checkAuth, nextRequestId, sendJson, type Ctx } from './src/http.ts';
import { handleMcp } from './src/mcp.ts';
import { handleRest } from './src/rest.ts';
import { Forge } from './src/store.ts';

export function createApp() {
  const forge = new Forge();
  const token = process.env.MAKEFUN_SHOP_TOKEN ?? '';
  const ctx: Ctx = { forge, token, reqSeq: { n: 0 } };

  const server = createServer((req: IncomingMessage, res: ServerResponse) => {
    handle(req, res, ctx).catch((error: unknown) => {
      try {
        process.stderr.write(`[entry] handler error: ${error instanceof Error ? error.message : String(error)}\n`);
        if (!res.headersSent) {
          sendJson(res, 500, { message: 'Internal Server Error' }, { 'x-github-request-id': nextRequestId(ctx) });
        } else {
          res.end();
        }
      } catch {
        // The socket is already gone; nothing more to do.
      }
    });
  });
  return server;
}

async function handle(req: IncomingMessage, res: ServerResponse, ctx: Ctx): Promise<void> {
  const url = new URL(req.url ?? '/', 'http://localhost');
  const pathname = url.pathname;
  const method = req.method ?? 'GET';

  if (method === 'GET' && pathname === '/healthz') {
    sendJson(res, 200, { ok: true });
    return;
  }

  if (pathname === '/api/graphql' || pathname.startsWith('/api/graphql/')) {
    if (method !== 'POST') {
      sendJson(res, 404, { message: 'Not Found' }, { 'x-github-request-id': nextRequestId(ctx) });
      return;
    }
    if (!checkAuth(req, ctx)) {
      sendJson(res, 401, { message: 'Unauthorized' }, { 'x-github-request-id': nextRequestId(ctx) });
      return;
    }
    await handleGraphQL(req, res, ctx);
    return;
  }

  if (pathname === '/mcp') {
    if (!checkAuth(req, ctx)) {
      const text = JSON.stringify({ jsonrpc: '2.0', id: null, error: { code: -32001, message: 'Unauthorized' } });
      res.writeHead(401, { 'content-type': 'application/json', 'content-length': Buffer.byteLength(text) });
      res.end(text);
      return;
    }
    await handleMcp(req, res, ctx);
    return;
  }

  if (pathname.startsWith('/api/v3')) {
    await handleRest(req, res, ctx);
    return;
  }

  if (/\.git\//.test(pathname) || pathname.endsWith('.git')) {
    await handleGit(req, res, ctx);
    return;
  }

  sendJson(res, 404, { message: 'Not Found' }, { 'x-github-request-id': nextRequestId(ctx) });
}
