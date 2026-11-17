// The git object model: hashing, tree/commit/tag parsing and building, and packfile read/write.
// Everything is plain objects over Buffers; no git binary is involved.

import { deflateSync, inflateSync } from 'node:zlib';
import { sha1 } from './util.ts';

export type GitObjectType = 'blob' | 'tree' | 'commit' | 'tag';

export interface GitObject {
  type: GitObjectType;
  data: Buffer;
}

export interface TreeEntry {
  mode: string;
  name: string;
  oid: string;
}

const TYPE_CODE: Record<GitObjectType, number> = { commit: 1, tree: 2, blob: 3, tag: 4 };
const CODE_TYPE: Record<number, GitObjectType> = { 1: 'commit', 2: 'tree', 3: 'blob', 4: 'tag' };

export function hashObject(type: GitObjectType, data: Buffer): string {
  return sha1(Buffer.concat([Buffer.from(`${type} ${data.length}\u0000`, 'utf8'), data]));
}

/** Read one object header (`<oid> <type> <size>`) from range bytes. */
export function objectKey(oid: string, type: GitObjectType, data: Buffer): Buffer {
  return Buffer.concat([Buffer.from(`${type} ${data.length}\u0000`, 'utf8'), data]);
}

export function parseTree(data: Buffer): TreeEntry[] {
  const entries: TreeEntry[] = [];
  let offset = 0;
  while (offset < data.length) {
    const space = data.indexOf(0x20, offset);
    if (space < 0) break;
    const nul = data.indexOf(0x00, space + 1);
    if (nul < 0 || nul + 21 > data.length) break;
    const mode = data.toString('ascii', offset, space);
    const name = data.toString('utf8', space + 1, nul);
    const oid = data.toString('hex', nul + 1, nul + 21);
    entries.push({ mode, name, oid });
    offset = nul + 21;
  }
  return entries;
}

/** Sort tree entries the way git does: byte order, a directory compared as name + '/'. */
function treeSortKey(entry: TreeEntry): string {
  return entry.mode === '40000' ? `${entry.name}/` : entry.name;
}

export function buildTree(entries: ReadonlyArray<TreeEntry>): Buffer {
  const sorted = [...entries].sort((a, b) => {
    const ka = treeSortKey(a);
    const kb = treeSortKey(b);
    return ka < kb ? -1 : ka > kb ? 1 : 0;
  });
  const parts: Buffer[] = [];
  for (const entry of sorted) {
    parts.push(Buffer.from(`${entry.mode} ${entry.name}\u0000`, 'utf8'));
    parts.push(Buffer.from(entry.oid, 'hex'));
  }
  return Buffer.concat(parts);
}

export interface CommitView {
  tree: string;
  parents: string[];
  author: string;
  committer: string;
  message: string;
  headers: string[];
}

export function parseCommit(data: Buffer): CommitView {
  const text = data.toString('binary');
  const split = text.indexOf('\n\n');
  const head = split < 0 ? text : text.slice(0, split);
  const message = split < 0 ? '' : data.subarray(split + 2).toString('utf8');
  const headers = head.split('\n').filter((line) => line !== '');
  let tree = '';
  const parents: string[] = [];
  let author = '';
  let committer = '';
  for (const line of headers) {
    if (line.startsWith('tree ')) tree = line.slice(5).trim();
    else if (line.startsWith('parent ')) parents.push(line.slice(7).trim());
    else if (line.startsWith('author ')) author = line.slice(7);
    else if (line.startsWith('committer ')) committer = line.slice(10);
  }
  return { tree, parents, author, committer, message, headers };
}

export interface CommitFields {
  tree: string;
  parents?: readonly string[];
  author: string;
  committer: string;
  message: string;
}

export function buildCommit(fields: CommitFields): Buffer {
  const lines: string[] = [`tree ${fields.tree}`];
  for (const parent of fields.parents ?? []) lines.push(`parent ${parent}`);
  lines.push(`author ${fields.author}`);
  lines.push(`committer ${fields.committer}`);
  const message = fields.message.endsWith('\n') ? fields.message : `${fields.message}\n`;
  return Buffer.from(`${lines.join('\n')}\n\n${message}`, 'utf8');
}

export interface TagView {
  object: string;
  type: string;
  tag: string;
  tagger: string;
  message: string;
}

export function parseTag(data: Buffer): TagView {
  const text = data.toString('binary');
  const split = text.indexOf('\n\n');
  const head = split < 0 ? text : text.slice(0, split);
  const message = split < 0 ? '' : data.subarray(split + 2).toString('utf8');
  let object = '';
  let type = '';
  let tag = '';
  let tagger = '';
  for (const line of head.split('\n')) {
    if (line.startsWith('object ')) object = line.slice(7).trim();
    else if (line.startsWith('type ')) type = line.slice(5).trim();
    else if (line.startsWith('tag ')) tag = line.slice(4).trim();
    else if (line.startsWith('tagger ')) tagger = line.slice(7);
  }
  return { object, type, tag, tagger, message };
}

/** A path-to-blob map flattened out of a tree, used by the contents and diff surfaces. */
export interface FlatEntry {
  mode: string;
  oid: string;
}

export function flattenTree(
  store: ReadonlyMap<string, GitObject>,
  treeOid: string,
  prefix = '',
  out: Map<string, FlatEntry> = new Map(),
  depth = 0,
): Map<string, FlatEntry> {
  if (depth > 40) return out;
  const tree = store.get(treeOid);
  if (tree === undefined || tree.type !== 'tree') return out;
  for (const entry of parseTree(tree.data)) {
    const path = prefix === '' ? entry.name : `${prefix}/${entry.name}`;
    if (entry.mode === '40000') flattenTree(store, entry.oid, path, out, depth + 1);
    else out.set(path, { mode: entry.mode, oid: entry.oid });
  }
  return out;
}

/** Build the nested trees for a flat path map; returns the root tree oid. */
export function buildTreeFromFlat(store: Map<string, GitObject>, files: ReadonlyMap<string, FlatEntry>): string {
  interface Node {
    files: Map<string, FlatEntry>;
    dirs: Map<string, Node>;
  }
  const make = (): Node => ({ files: new Map(), dirs: new Map() });
  const root = make();
  for (const [path, entry] of files) {
    const segments = path.split('/').filter((part) => part !== '');
    if (segments.length === 0) continue;
    let node = root;
    for (let index = 0; index < segments.length - 1; index += 1) {
      const name = segments[index];
      let child = node.dirs.get(name);
      if (child === undefined) {
        child = make();
        node.dirs.set(name, child);
      }
      node = child;
    }
    node.files.set(segments[segments.length - 1], entry);
  }
  const put = (type: GitObjectType, data: Buffer): string => {
    const oid = hashObject(type, data);
    if (!store.has(oid)) store.set(oid, { type, data });
    return oid;
  };
  const build = (node: Node): string => {
    const entries: TreeEntry[] = [];
    for (const [name, entry] of node.files) entries.push({ mode: entry.mode, name, oid: entry.oid });
    for (const [name, child] of node.dirs) entries.push({ mode: '40000', name, oid: build(child) });
    return put('tree', buildTree(entries));
  };
  return build(root);
}

// ---------------------------------------------------------------- packfile reading

export function applyDelta(base: Buffer, delta: Buffer): Buffer {
  let offset = 0;
  const readVarint = (): number => {
    let value = 0;
    let shift = 0;
    for (;;) {
      const byte = delta[offset++];
      value += (byte & 0x7f) * 2 ** shift;
      if ((byte & 0x80) === 0) break;
      shift += 7;
    }
    return value;
  };
  readVarint();
  const targetSize = readVarint();
  const out = Buffer.alloc(targetSize);
  let written = 0;
  while (offset < delta.length) {
    const opcode = delta[offset++];
    if ((opcode & 0x80) !== 0) {
      let copyOffset = 0;
      let copySize = 0;
      if (opcode & 0x01) copyOffset |= delta[offset++];
      if (opcode & 0x02) copyOffset |= delta[offset++] << 8;
      if (opcode & 0x04) copyOffset |= delta[offset++] << 16;
      if (opcode & 0x08) copyOffset |= delta[offset++] << 24;
      if (opcode & 0x10) copySize |= delta[offset++];
      if (opcode & 0x20) copySize |= delta[offset++] << 8;
      if (opcode & 0x40) copySize |= delta[offset++] << 16;
      if (copySize === 0) copySize = 0x10000;
      const chunk = base.subarray(copyOffset, copyOffset + copySize);
      chunk.copy(out, written);
      written += chunk.length;
    } else if (opcode !== 0) {
      const count = opcode;
      delta.copy(out, written, offset, offset + count);
      written += count;
      offset += count;
    } else {
      break;
    }
  }
  return written === out.length ? out : out.subarray(0, written);
}

export interface PackEntry {
  type: GitObjectType;
  data: Buffer;
  oid: string;
  offset: number;
  delta: boolean;
}

export interface PackReadResult {
  version: number;
  declaredCount: number;
  entries: PackEntry[];
  objects: Map<string, GitObject>;
  deltaCount: number;
}

/** Read a packfile, resolving OFS_DELTA and REF_DELTA. `external` supplies REF_DELTA bases off-pack. */
export function readPack(pack: Buffer, external?: (oid: string) => GitObject | undefined): PackReadResult {
  if (pack.length < 32 || pack.toString('ascii', 0, 4) !== 'PACK') return { version: 0, declaredCount: 0, entries: [], objects: new Map(), deltaCount: 0 };
  const version = pack.readUInt32BE(4);
  const declaredCount = pack.readUInt32BE(8);
  const body = pack.subarray(0, pack.length - 20);
  const entries: PackEntry[] = [];
  const objects = new Map<string, GitObject>();
  const byOffset = new Map<number, GitObject>();
  let deltaCount = 0;
  let offset = 12;
  for (let index = 0; index < declaredCount && offset < body.length; index += 1) {
    const start = offset;
    let byte = body[offset++];
    const code = (byte >> 4) & 0x07;
    let size = byte & 0x0f;
    let shift = 4;
    while ((byte & 0x80) !== 0) {
      byte = body[offset++];
      size += (byte & 0x7f) * 2 ** shift;
      shift += 7;
    }
    let baseObject: GitObject | undefined;
    let baseOid: string | undefined;
    if (code === 6) {
      // OFS_DELTA: a negative offset varint to an earlier object in this pack.
      let byte2 = body[offset++];
      let value = byte2 & 0x7f;
      while ((byte2 & 0x80) !== 0) {
        byte2 = body[offset++];
        value = (value + 1) * 128 + (byte2 & 0x7f);
      }
      baseObject = byOffset.get(start - value);
      deltaCount += 1;
    } else if (code === 7) {
      // REF_DELTA: the base's full oid.
      baseOid = body.toString('hex', offset, offset + 20);
      offset += 20;
      baseObject = objects.get(baseOid) ?? external?.(baseOid);
      deltaCount += 1;
    }
    let inflated: Buffer;
    try {
      inflated = inflateSync(body.subarray(offset));
    } catch {
      break;
    }
    // The compressed stream's length is not returned by inflateSync in older Node, so scan zlib
    // framing: re-inflate with the info option where available, else walk the stream.
    const consumed = zlibConsumed(body, offset, inflated.length, size);
    offset += consumed;
    let type: GitObjectType;
    let data: Buffer;
    if (code === 6 || code === 7) {
      if (baseObject === undefined) break;
      type = baseObject.type;
      data = applyDelta(baseObject.data, inflated);
    } else {
      type = CODE_TYPE[code] ?? 'blob';
      data = inflated;
    }
    const oid = hashObject(type, data);
    const object: GitObject = { type, data };
    objects.set(oid, object);
    byOffset.set(start, object);
    entries.push({ type, data, oid, offset: start, delta: code === 6 || code === 7 });
  }
  return { version, declaredCount, entries, objects, deltaCount };
}

/** How many bytes of the zlib stream at `offset` belong to it: decompress incrementally without output. */
function zlibConsumed(buffer: Buffer, offset: number, inflatedSize: number, hint: number): number {
  // Fast path: zlib stores an Adler-32 trailer; inflateSync's `info` exposes bytesWritten in Node.
  try {
    const result = inflateSync(buffer.subarray(offset), { info: true } as never) as unknown as { engine?: { bytesWritten?: number } };
    const written = result.engine?.bytesWritten;
    if (typeof written === 'number' && written > 0) return written;
  } catch {
    // fall through
  }
  // Fallback: the deflate stream is at most the whole rest plus a couple of header bytes. Walk it.
  let best = Math.min(buffer.length - offset, 32 + inflatedSize);
  while (best <= buffer.length - offset) {
    try {
      const attempt = inflateSync(buffer.subarray(offset, offset + best));
      if (attempt.length === inflatedSize) return best;
    } catch {
      // not enough bytes
    }
    best += 16;
  }
  return Math.max(0, hint);
}

// ---------------------------------------------------------------- packfile writing

function writeObjectHeader(type: GitObjectType, size: number): Buffer {
  const bytes: number[] = [];
  let remaining = size;
  let first = (TYPE_CODE[type] << 4) | (remaining & 0x0f);
  remaining = Math.floor(remaining / 16);
  while (remaining > 0) {
    bytes.push(first | 0x80);
    first = remaining & 0x7f;
    remaining = Math.floor(remaining / 128);
  }
  bytes.push(first);
  return Buffer.from(bytes);
}

/** Build a version-2 packfile of plain objects (no deltas), with its SHA-1 trailer. */
export function writePack(objects: Iterable<GitObject>): Buffer {
  const list = [...objects];
  const parts: Buffer[] = [Buffer.from('PACK', 'ascii'), Buffer.alloc(8)];
  parts[1].writeUInt32BE(2, 0);
  parts[1].writeUInt32BE(list.length, 4);
  for (const object of list) {
    parts.push(writeObjectHeader(object.type, object.data.length));
    parts.push(deflateSync(object.data));
  }
  const body = Buffer.concat(parts);
  return Buffer.concat([body, Buffer.from(sha1(body), 'hex')]);
}

/** Recompute a pack's trailer; returns whether it matches and both values. */
export function verifyPackChecksum(pack: Buffer): { ok: boolean; expected: string; actual: string } {
  if (pack.length < 20) return { ok: false, expected: '', actual: '' };
  const body = pack.subarray(0, pack.length - 20);
  const expected = sha1(body);
  const actual = pack.toString('hex', pack.length - 20);
  return { ok: expected === actual, expected, actual };
}
