// The server: one raw TCP listener over the single in-memory store. Node's http module is replaced
// with a hand-rolled HTTP/1.1 parser and a synchronous dispatcher so the four timed workloads pay no
// IncomingMessage/ServerResponse allocations, no promise hop and no URL object per request.

import { createServer, type Server, type Socket } from 'node:net';
import { URLSearchParams } from 'node:url';
import type { IncomingMessage, ServerResponse } from 'node:http';
import { handleInfoRefs, handleReceivePack, handleUploadPack } from './git.ts';
import { handleGraphql } from './graphql.ts';
import { handleMcp } from './mcp.ts';
import { handleRest } from './rest.ts';
import { findRepo, store } from './store.ts';
import { JSON_TYPE, safeDecode, sendJson, sendText } from './util.ts';

const PREFIX = '/api/v3';
const CRLFCRLF = Buffer.from('\r\n\r\n', 'latin1');
const REASONS: readonly string[] = [
  'OK', 'OK', 'Created', 'Accepted', 'No Content', 'Bad Request', 'Unauthorized', 'OK',
  'Conflict', 'Gone', 'Precondition Failed', 'Unprocessable Entity', 'Too Many Requests',
  'Internal Server Error', 'Not Implemented', 'Service Unavailable',
];

interface FastRequest {
  method: string;
  url: string;
  headers: Record<string, string>;
}

interface FastResponse {
  headersSent: boolean;
  writeHead(status: number, headers?: Record<string, string> | undefined): void;
  end(body?: string | Buffer | undefined): void;
}

function requestId(): string {
  store.requestSeq += 1;
  return `MF-${store.requestSeq.toString(16).padStart(8, '0')}`;
}

function tokenOf(req: FastRequest): string {
  const header = req.headers.authorization;
  if (header === undefined) return '';
  const match = /^(?:Bearer|token)\s+(.+)$/i.exec(header.trim());
  return match === null ? '' : (match[1] as string).trim();
}

function authenticated(req: FastRequest): boolean {
  const expected = process.env.MAKEFUN_SHOP_TOKEN ?? '';
  if (expected === '') return true;
  return tokenOf(req) === expected;
}

function unauthorized(res: FastResponse): void {
  sendJson(res as never, 401, { message: 'Requires authentication', documentation_url: 'https://docs.github.com/rest' }, { 'x-github-request-id': requestId() });
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

/** Fully synchronous: the body is already in memory, so no await and no event hop. */
function dispatch(req: FastRequest, res: FastResponse, body: Buffer): void {
  const method = req.method;
  const { pathname, query } = splitTarget(req.url);

  if (pathname === '/healthz') {
    sendText(res as never, 200, 'ok\n');
    return;
  }

  const git = gitRoute(pathname);
  if (git !== null) {
    const repo = findRepo(git.owner, git.repo);
    if (repo === undefined) {
      sendText(res as never, 404, 'repository not found\n');
      return;
    }
    if (git.action === 'info/refs') {
      const service = queryParam(query, 'service');
      if (service !== 'git-upload-pack' && service !== 'git-receive-pack') {
        sendText(res as never, 400, 'unsupported service\n');
        return;
      }
      handleInfoRefs(req as never as IncomingMessage, res as never as ServerResponse, repo, service);
      return;
    }
    if (git.action === 'git-upload-pack') {
      handleUploadPack(req as never as IncomingMessage, res as never as ServerResponse, repo, body);
      return;
    }
    handleReceivePack(req as never as IncomingMessage, res as never as ServerResponse, repo, body);
    return;
  }

  if (pathname === '/api/graphql' || pathname === '/api/graphql/') {
    if (!authenticated(req)) {
      unauthorized(res);
      return;
    }
    if (method !== 'POST') {
      sendJson(res as never, 405, { message: 'Method not allowed' });
      return;
    }
    handleGraphql(res as never as ServerResponse, body.toString('utf8'));
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
      sendJson(res as never, 405, { message: 'Method not allowed' });
      return;
    }
    if (body.length === 0) {
      sendJson(res as never, 400, { jsonrpc: '2.0', id: null, error: { code: -32700, message: 'Parse error: empty body' } });
      return;
    }
    handleMcp(res as never as ServerResponse, body.toString('utf8'));
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
    handleRest(req as never as IncomingMessage, res as never as ServerResponse, {
      method,
      segments,
      query: query === '' ? EMPTY_QUERY : new URLSearchParams(query),
      bodyText: body.toString('utf8'),
      host: req.headers.host ?? 'localhost',
    });
    return;
  }

  sendJson(res as never, 404, { message: 'Not Found', documentation_url: 'https://docs.github.com/rest' }, { 'x-github-request-id': requestId() });
}

/** One response, one write. Header text is built without intermediate arrays. */
class Response implements FastResponse {
  headersSent = false;
  private status = 200;
  private headers: Record<string, string> | undefined;
  private closeAfter: boolean;

  private readonly socket: Socket;
  private readonly head: boolean;

  constructor(socket: Socket, head: boolean, closeAfter: boolean) {
    this.socket = socket;
    this.head = head;
    this.closeAfter = closeAfter;
  }

  writeHead(status: number, headers?: Record<string, string> | undefined): void {
    this.status = status;
    this.headers = headers;
    return undefined;
  }

  end(body?: string | Buffer | undefined): void {
    if (this.headersSent) return;
    this.headersSent = true;
    const bodyBuffer = this.head || body === undefined ? null : typeof body === 'string' ? Buffer.from(body, 'utf8') : body;
    const reason = REASONS[this.status] ?? 'OK';
    let header = `HTTP/1.1 ${this.status} ${reason}\r\n`;
    const h = this.headers;
    let hasConnection = false;
    let hasLength = false;
    if (h !== undefined) {
      for (const key of Object.keys(h)) {
        if (key === 'connection') hasConnection = true;
        if (key === 'content-length') hasLength = true;
        header += `${key}: ${h[key]}\r\n`;
      }
    }
    if (!hasLength) header += `content-length: ${bodyBuffer === null ? 0 : bodyBuffer.length}\r\n`;
    if (!hasConnection) header += this.closeAfter ? 'connection: close\r\n' : 'connection: keep-alive\r\n';
    header += '\r\n';
    const out = Buffer.allocUnsafe(header.length + (bodyBuffer === null ? 0 : bodyBuffer.length));
    out.write(header, 0, 'latin1');
    if (bodyBuffer !== null) bodyBuffer.copy(out, header.length);
    if (this.closeAfter) this.socket.end(out);
    else this.socket.write(out);
  }
}

/** Build the listener. main.ts binds it to an ephemeral port and registers it with the env. */
export function createApp(): Server {
  return createServer((socket) => {
    socket.setNoDelay(true);
    socket.setKeepAlive(true, 65_000);
    let buf = Buffer.allocUnsafe(16 * 1024);
    let used = 0;
    let done = false;

    socket.on('data', (chunk: Buffer) => {
      if (done) return;
      if (used + chunk.length > buf.length) {
        const bigger = Buffer.allocUnsafe(Math.max(buf.length * 2, used + chunk.length));
        buf.copy(bigger, 0, 0, used);
        buf = bigger;
      }
      chunk.copy(buf, used);
      used += chunk.length;

      for (;;) {
        const headerEnd = findHeaderEnd(buf, used);
        if (headerEnd === -1) break;
        const head = buf.toString('latin1', 0, headerEnd);
        const lines = head.split('\r\n');
        const requestLine = lines[0] ?? '';
        const parts = requestLine.split(' ');
        const method = parts[0] ?? 'GET';
        const target = parts[1] ?? '/';
        if (method === '' || target === '') {
          const res = new Response(socket, false, true);
          sendText(res as never, 400, 'bad request\n');
          done = true;
          return;
        }
        const headers: Record<string, string> = Object.create(null) as Record<string, string>;
        for (let i = 1; i < lines.length; i += 1) {
          const line = lines[i] ?? '';
          const colon = line.indexOf(':');
          if (colon > 0) headers[line.slice(0, colon).toLowerCase()] = line.slice(colon + 1).trim();
        }
        const lengthText = headers['content-length'];
        const bodyLength = lengthText === undefined ? 0 : Number.parseInt(lengthText, 10) || 0;
        const bodyStart = headerEnd + 4;
        if (used < bodyStart + bodyLength) break;

        if (headers['expect'] === '100-continue') socket.write('HTTP/1.1 100 Continue\r\n\r\n');
        const body = bodyLength === 0 ? EMPTY_BODY : buf.subarray(bodyStart, bodyStart + bodyLength);
        const consumed = bodyStart + bodyLength;
        const closeAfter = headers['connection'] === 'close' || target.startsWith('http://');
        const res = new Response(socket, method === 'HEAD', closeAfter);
        try {
          dispatch({ method, url: target, headers }, res, body);
        } catch (err) {
          if (!res.headersSent) {
            sendJson(res as never, 500, { message: `internal error: ${err instanceof Error ? err.message : String(err)}` }, { 'x-github-request-id': requestId() });
          } else if (closeAfter) {
            socket.end();
          }
        }
        if (!res.headersSent) res.end();
        used -= consumed;
        if (used > 0) buf.copy(buf, 0, consumed, consumed + used);
        if (closeAfter) {
          done = true;
          return;
        }
      }
    });
    socket.on('error', () => { done = true; });
    socket.on('close', () => { done = true; });
  });
}

const EMPTY_BODY = Buffer.alloc(0);

function findHeaderEnd(buffer: Buffer, length: number): number {
  for (let i = 3; i < length; i += 1) {
    if (buffer[i] === 10 && buffer[i - 1] === 13 && buffer[i - 2] === 10 && buffer[i - 3] === 13) return i - 3;
  }
  return -1;
}
