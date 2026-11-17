// The server: one HTTP listener over the single in-memory store, dispatching the four scored
// surfaces - git smart HTTP, REST v3, GraphQL v4 and MCP - plus the boot health probe. main.ts owns
// the register/done lifecycle; nothing here reads a clock or binds a fixed port.

import { createServer, type IncomingMessage, type Server, type ServerResponse } from 'node:http';
import { URLSearchParams } from 'node:url';
import { handleInfoRefs, handleReceivePack, handleUploadPack } from './git.ts';
import { handleGraphql } from './graphql.ts';
import { handleMcp } from './mcp.ts';
import { handleRest } from './rest.ts';
import { findRepo, store } from './store.ts';
import { JSON_TYPE, safeDecode, sendJson, sendText } from './util.ts';

const PREFIX = '/api/v3';

function requestId(): string {
  store.requestSeq += 1;
  return `MF-${store.requestSeq.toString(16).padStart(8, '0')}`;
}

function tokenOf(req: IncomingMessage): string {
  const header = req.headers.authorization;
  if (typeof header !== 'string') return '';
  const match = /^(?:Bearer|token)\s+(.+)$/i.exec(header.trim());
  return match === null ? '' : (match[1] as string).trim();
}

function authenticated(req: IncomingMessage): boolean {
  const expected = process.env.MAKEFUN_SHOP_TOKEN ?? '';
  if (expected === '') return true;
  return tokenOf(req) === expected;
}

function unauthorized(res: ServerResponse): void {
  sendJson(res, 401, { message: 'Requires authentication', documentation_url: 'https://docs.github.com/rest' }, { 'x-github-request-id': requestId() });
}

function readBody(req: IncomingMessage): Promise<Buffer> {
  return new Promise((resolve, reject) => {
    const chunks: Buffer[] = [];
    req.on('data', (chunk: Buffer) => chunks.push(chunk));
    req.on('end', () => resolve(Buffer.concat(chunks)));
    req.on('error', reject);
  });
}

function gitRoute(pathname: string): { owner: string; repo: string; action: string } | null {
  const match = /^\/([^/]+)\/([^/]+)\.git\/(info\/refs|git-upload-pack|git-receive-pack)$/.exec(pathname);
  if (match === null) return null;
  return {
    owner: safeDecode(match[1] as string),
    repo: safeDecode(match[2] as string),
    action: match[3] as string,
  };
}

const EMPTY_QUERY = new URLSearchParams();

/** Split a request target without building a full URL object on every request. */
function splitTarget(raw: string): { pathname: string; query: string } {
  const q = raw.indexOf('?');
  if (q === -1) return { pathname: raw, query: '' };
  const hash = raw.indexOf('#', q + 1);
  return { pathname: raw.slice(0, q), query: hash === -1 ? raw.slice(q + 1) : raw.slice(q + 1, hash) };
}

/** Pull one query parameter out of a raw query string without allocating URLSearchParams. */
function queryParam(query: string, name: string): string {
  if (query === '') return '';
  let from = 0;
  while (from <= query.length) {
    const amp = query.indexOf('&', from);
    const pair = amp === -1 ? query.slice(from) : query.slice(from, amp);
    const eq = pair.indexOf('=');
    if ((eq === -1 ? pair : pair.slice(0, eq)) === name) {
      const value = eq === -1 ? '' : pair.slice(eq + 1);
      try { return decodeURIComponent(value.replace(/\+/g, ' ')); } catch { return value; }
    }
    if (amp === -1) break;
    from = amp + 1;
  }
  return '';
}

async function dispatch(req: IncomingMessage, res: ServerResponse): Promise<void> {
  const method = (req.method ?? 'GET').toUpperCase();
  const { pathname, query } = splitTarget(req.url ?? '/');

  if (pathname === '/healthz') {
    sendText(res, 200, 'ok\n');
    return;
  }

  // Every GET/HEAD in this protocol is bodyless; skipping the stream listeners keeps the
  // 2,000-read workload on a single tight path.
  const body = method === 'GET' || method === 'HEAD' ? Buffer.alloc(0) : await readBody(req);

  const git = gitRoute(pathname);
  if (git !== null) {
    const repo = findRepo(git.owner, git.repo);
    if (repo === undefined) {
      sendText(res, 404, 'repository not found\n');
      return;
    }
    if (git.action === 'info/refs') {
      const service = queryParam(query, 'service');
      if (service !== 'git-upload-pack' && service !== 'git-receive-pack') {
        sendText(res, 400, 'unsupported service\n');
        return;
      }
      handleInfoRefs(req, res, repo, service);
      return;
    }
    if (git.action === 'git-upload-pack') {
      handleUploadPack(req, res, repo, body);
      return;
    }
    handleReceivePack(req, res, repo, body);
    return;
  }

  if (pathname === '/api/graphql' || pathname === '/api/graphql/') {
    if (!authenticated(req)) {
      unauthorized(res);
      return;
    }
    if (method !== 'POST') {
      sendJson(res, 405, { message: 'Method not allowed' });
      return;
    }
    handleGraphql(res, body.toString('utf8'));
    return;
  }

  if (pathname === '/mcp' || pathname === '/mcp/') {
    if (!authenticated(req)) {
      unauthorized(res);
      return;
    }
    if (method !== 'POST') {
      if (method === 'DELETE') {
        res.writeHead(204);
        res.end();
        return;
      }
      sendJson(res, 405, { message: 'Method not allowed' });
      return;
    }
    if (body.length === 0) {
      sendJson(res, 400, { jsonrpc: '2.0', id: null, error: { code: -32700, message: 'Parse error: empty body' } });
      return;
    }
    handleMcp(res, body.toString('utf8'));
    return;
  }

  if (pathname === PREFIX || pathname.startsWith(`${PREFIX}/`)) {
    if (!authenticated(req)) {
      unauthorized(res);
      return;
    }
    const residual = pathname.slice(PREFIX.length);
    const trimmed = residual.startsWith('/') ? residual.slice(1) : residual;
    const segments = trimmed === '' ? [] : trimmed.split('/').map(safeDecode);
    handleRest(req, res, {
      method,
      segments,
      query: query === '' ? EMPTY_QUERY : new URLSearchParams(query),
      bodyText: body.toString('utf8'),
      host: req.headers.host ?? 'localhost',
    });
    return;
  }

  sendJson(res, 404, { message: 'Not Found', documentation_url: 'https://docs.github.com/rest' }, { 'x-github-request-id': requestId() });
}

/** Build the listener. main.ts binds it to an ephemeral port and registers it with the env. */
export function createApp(): Server {
  const server = createServer((req, res) => {
    dispatch(req, res).catch((err: unknown) => {
      if (res.headersSent) {
        res.end();
        return;
      }
      sendJson(
        res,
        500,
        { message: `internal error: ${err instanceof Error ? err.message : String(err)}` },
        { 'x-github-request-id': requestId(), 'content-type': JSON_TYPE },
      );
    });
  });
  server.keepAliveTimeout = 65_000;
  server.headersTimeout = 66_000;
  server.requestTimeout = 0;
  server.maxRequestsPerSocket = 0;
  return server;
}
