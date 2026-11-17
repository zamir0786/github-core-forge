// Small shared helpers: hashing, base64, pkt-line framing, time and JSON/HTTP odds and ends.

import { createHash } from 'node:crypto';
import type { ServerResponse } from 'node:http';

export const ZERO_OID = '0'.repeat(40);
export const JSON_TYPE = 'application/json; charset=utf-8';

export function sha1(data: Buffer | Uint8Array | string): string {
  const hash = createHash('sha1');
  hash.update(data as never);
  return hash.digest('hex');
}

export function sha256(data: Buffer | Uint8Array | string): string {
  const hash = createHash('sha256');
  hash.update(data as never);
  return hash.digest('hex');
}

export function b64(data: Buffer | Uint8Array | string): string {
  return Buffer.from(data as never).toString('base64');
}

export function unb64(text: string): Buffer {
  return Buffer.from(text, 'base64');
}

export function toB64Url(text: string): string {
  return Buffer.from(text, 'utf8').toString('base64').replace(/\+/g, '-').replace(/\//g, '_').replace(/=+$/, '');
}

export function fromB64Url(text: string): string | null {
  try {
    return Buffer.from(text.replace(/-/g, '+').replace(/_/g, '/'), 'base64').toString('utf8');
  } catch {
    return null;
  }
}

/** One pkt-line: a 4-hex length prefix (counting itself) and the payload. */
export function pkt(payload: string | Buffer): Buffer {
  const body = typeof payload === 'string' ? Buffer.from(payload, 'utf8') : payload;
  const length = body.length + 4;
  return Buffer.concat([Buffer.from(length.toString(16).padStart(4, '0'), 'ascii'), body]);
}

export const FLUSH = Buffer.from('0000', 'ascii');
export const DELIM = Buffer.from('0001', 'ascii');

export function pktLines(parts: ReadonlyArray<string | Buffer | 'flush' | 'delim'>): Buffer {
  const buffers: Buffer[] = [];
  for (const part of parts) {
    if (part === 'flush') buffers.push(FLUSH);
    else if (part === 'delim') buffers.push(DELIM);
    else buffers.push(pkt(part));
  }
  return Buffer.concat(buffers);
}

export interface Packet {
  kind: 'data' | 'flush' | 'delim' | 'bad';
  payload: Buffer;
  raw: Buffer;
  next: number;
}

/** Parse one pkt-line at `offset`; `bad` when the length prefix is not hexadecimal. */
export function readPacket(buffer: Buffer, offset: number): Packet {
  if (offset + 4 > buffer.length) return { kind: 'bad', payload: Buffer.alloc(0), raw: buffer.subarray(offset), next: buffer.length };
  const header = buffer.toString('ascii', offset, offset + 4);
  if (!/^[0-9a-fA-F]{4}$/.test(header)) return { kind: 'bad', payload: Buffer.alloc(0), raw: buffer.subarray(offset, offset + 4), next: offset + 4 };
  const length = parseInt(header, 16);
  if (length === 0) return { kind: 'flush', payload: Buffer.alloc(0), raw: buffer.subarray(offset, offset + 4), next: offset + 4 };
  if (length === 1) return { kind: 'delim', payload: Buffer.alloc(0), raw: buffer.subarray(offset, offset + 4), next: offset + 4 };
  if (length < 4 || offset + length > buffer.length) return { kind: 'bad', payload: Buffer.alloc(0), raw: buffer.subarray(offset), next: buffer.length };
  return { kind: 'data', payload: buffer.subarray(offset + 4, offset + length), raw: buffer.subarray(offset, offset + length), next: offset + length };
}

/** The data payloads of a pkt-line stream, stopping at the first malformed packet. */
export function packetsOf(buffer: Buffer): Buffer[] {
  const out: Buffer[] = [];
  let offset = 0;
  while (offset < buffer.length) {
    const packet = readPacket(buffer, offset);
    offset = packet.next;
    if (packet.kind === 'data') out.push(packet.payload);
    else if (packet.kind === 'bad') break;
  }
  return out;
}

const EPOCH_MS = 1_766_000_000_000; // 2025-12-15T08:53:20Z, fixed so nothing reads a wall clock
let clockTick = 0;

export function nowMs(): number {
  const raw = process.env.MAKEFUN_TRIAL_NOW;
  if (raw !== undefined && raw !== '') {
    const parsed = Date.parse(raw);
    if (Number.isFinite(parsed)) return parsed;
    const numeric = Number(raw);
    if (Number.isFinite(numeric) && numeric > 0) return numeric < 1e12 ? numeric * 1000 : numeric;
  }
  return EPOCH_MS;
}

export function tick(): number {
  clockTick += 1;
  return nowMs() + clockTick * 1000;
}

export function iso(ms: number): string {
  return new Date(ms).toISOString().replace(/\.\d{3}Z$/, 'Z');
}

export function isoNow(): string {
  return iso(tick());
}

/** The identity line git uses inside commits. */
export function gitIdent(name: string, email: string, ms: number): string {
  return `${name} <${email}> ${Math.floor(ms / 1000)} +0000`;
}

export function sendJson(res: ServerResponse, status: number, body: unknown, headers: Readonly<Record<string, string>> = {}): void {
  const text = `${JSON.stringify(body)}\n`;
  res.writeHead(status, { 'content-type': JSON_TYPE, 'content-length': Buffer.byteLength(text), ...headers });
  res.end(text);
}

export function sendText(res: ServerResponse, status: number, text: string, contentType = 'text/plain; charset=utf-8', headers: Readonly<Record<string, string>> = {}): void {
  res.writeHead(status, { 'content-type': contentType, 'content-length': Buffer.byteLength(text), ...headers });
  res.end(text);
}

export function sendBuffer(res: ServerResponse, status: number, body: Buffer, contentType: string, headers: Readonly<Record<string, string>> = {}): void {
  res.writeHead(status, { 'content-type': contentType, 'content-length': body.length, ...headers });
  res.end(body);
}

/** A rejected promise is never left dangling by a request handler. */
export function safeDecode(text: string): string {
  try {
    return decodeURIComponent(text);
  } catch {
    return text;
  }
}

export function lower(text: string): string {
  return text.toLowerCase();
}

export function sameName(a: string, b: string): boolean {
  return a.toLowerCase() === b.toLowerCase();
}
