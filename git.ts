// Git smart HTTP: protocol v2 upload-pack (ls-refs and fetch), the v0 receive-pack push path,
// and the info/refs advertisements. No external git process is involved.

import type { IncomingMessage, ServerResponse } from 'node:http';
import { parseCommit, parseTag, parseTree, readPack, writePack, type GitObject } from './objects.ts';
import { commitOf, putObject, type Repo } from './store.ts';
import { FLUSH, pkt, pktLines, readPacket, ZERO_OID } from './util.ts';

const UPLOAD_TYPE = 'application/x-git-upload-pack-advertisement';
const UPLOAD_RESULT = 'application/x-git-upload-pack-result';
const RECEIVE_TYPE = 'application/x-git-receive-pack-advertisement';
const RECEIVE_RESULT = 'application/x-git-receive-pack-result';
const AGENT = 'makefun-forge/1.0';

function headOid(repo: Repo): string | undefined {
  return repo.refs.get(`refs/heads/${repo.defaultBranch}`);
}

function sortedRefs(repo: Repo): Array<[string, string]> {
  return [...repo.refs.entries()].sort((a, b) => (a[0] < b[0] ? -1 : a[0] > b[0] ? 1 : 0));
}

function peelOid(repo: Repo, oid: string): string | undefined {
  const object = repo.objects.get(oid);
  if (object === undefined || object.type !== 'tag') return undefined;
  return commitOf(repo, oid);
}

interface CacheEntry {
  sig: string;
  body: Buffer;
}

const gitCache = new WeakMap<Repo, Map<string, CacheEntry>>();

function refSig(repo: Repo): string {
  let sig = repo.defaultBranch;
  for (const [ref, oid] of sortedRefs(repo)) sig += `;${ref}=${oid}`;
  return sig;
}

/** Memoize one deterministic response until the repo's refs change. */
function cached(repo: Repo, key: string, make: () => Buffer): Buffer {
  const sig = refSig(repo);
  let map = gitCache.get(repo);
  if (map === undefined) {
    map = new Map();
    gitCache.set(repo, map);
  }
  const hit = map.get(key);
  if (hit !== undefined && hit.sig === sig) return hit.body;
  const body = make();
  map.set(key, { sig, body });
  return body;
}

/** `GET /<owner>/<repo>.git/info/refs?service=...`. */
export function handleInfoRefs(req: IncomingMessage, res: ServerResponse, repo: Repo, service: string): void {
  const header = String(req.headers['git-protocol'] ?? '');
  const v2 = service === 'git-upload-pack' && header.includes('version=2');
  const isUpload = service === 'git-upload-pack';
  const body = cached(repo, `ad:${isUpload}:${v2}`, () => {
    if (isUpload && v2) {
      return pktLines([
        '# service=git-upload-pack\n',
        'flush',
        'version 2\n',
        `agent=${AGENT}\n`,
        'ls-refs=unborn\n',
        'fetch=shallow\n',
        'object-format=sha1\n',
        'flush',
      ]);
    }
    const tip = headOid(repo);
    const lines: Array<string | 'flush'> = [];
    if (isUpload) {
      const caps = `multi_ack_detailed side-band-64k thin-pack ofs-delta shallow symref=HEAD:refs/heads/${repo.defaultBranch} agent=${AGENT} object-format=sha1`;
      lines.push('# service=git-upload-pack\n', 'flush');
      if (tip === undefined) lines.push(`${ZERO_OID} capabilities^{}\u0000${caps}\n`);
      else {
        lines.push(`${tip} HEAD\u0000${caps}\n`);
        for (const [ref, oid] of sortedRefs(repo)) {
          lines.push(`${oid} ${ref}\n`);
          const peeled = peelOid(repo, oid);
          if (peeled !== undefined) lines.push(`${peeled} ${ref}^{}\n`);
        }
      }
    } else {
      // receive-pack is always protocol v0.
      const caps = `report-status side-band-64k delete-refs ofs-delta agent=${AGENT} symref=HEAD:refs/heads/${repo.defaultBranch}`;
      lines.push('# service=git-receive-pack\n', 'flush');
      if (tip === undefined) lines.push(`${ZERO_OID} capabilities^{}\u0000${caps}\n`);
      else {
        lines.push(`${tip} HEAD\u0000${caps}\n`);
        for (const [ref, oid] of sortedRefs(repo)) lines.push(`${oid} ${ref}\n`);
      }
    }
    lines.push('flush');
    return pktLines(lines);
  });
  const type = isUpload ? UPLOAD_TYPE : RECEIVE_TYPE;
  res.writeHead(200, { 'content-type': type, 'content-length': body.length, 'cache-control': 'no-cache' });
  res.end(body);
}

function sideband(data: Buffer, chunkSize = 65000): Buffer {
  const parts: Buffer[] = [];
  for (let offset = 0; offset < data.length; offset += chunkSize) {
    const chunk = data.subarray(offset, Math.min(offset + chunkSize, data.length));
    parts.push(pkt(Buffer.concat([Buffer.from([1]), chunk])));
  }
  return Buffer.concat(parts);
}

/** The whole object closure of one oid, optionally stopping commit ancestry at boundaries. */
function closure(repo: Repo, roots: readonly string[], stopAt: ReadonlySet<string> | undefined, out: Map<string, GitObject>): void {
  const stack = [...roots];
  const seen = new Set<string>();
  while (stack.length > 0) {
    const oid = stack.pop() as string;
    if (seen.has(oid)) continue;
    seen.add(oid);
    const object = repo.objects.get(oid);
    if (object === undefined) continue;
    out.set(oid, object);
    if (object.type === 'tree') {
      for (const entry of parseTree(object.data)) stack.push(entry.oid);
    } else if (object.type === 'commit') {
      const view = parseCommit(object.data);
      stack.push(view.tree);
      if (stopAt !== undefined && stopAt.has(oid)) continue;
      for (const parent of view.parents) stack.push(parent);
    } else if (object.type === 'tag') {
      stack.push(parseTag(object.data).object);
    }
  }
}

interface FetchPlan {
  objects: Map<string, GitObject>;
  shallow: string[];
  unshallow: string[];
}

function planFetch(repo: Repo, wants: readonly string[], haves: readonly string[], depth: number | undefined, clientShallow: readonly string[]): FetchPlan {
  const result = new Map<string, GitObject>();
  const newShallow: string[] = [];
  if (wants.length === 0) return { objects: result, shallow: newShallow, unshallow: [] };

  if (depth !== undefined && depth > 0) {
    const queue: Array<{ oid: string; gen: number }> = [];
    const commitGen = new Map<string, number>();
    const extras: string[] = [];
    for (const want of wants) {
      const object = repo.objects.get(want);
      if (object === undefined) continue;
      if (object.type === 'tag') {
        result.set(want, object);
        const peeled = commitOf(repo, want);
        if (peeled !== undefined) queue.push({ oid: peeled, gen: 1 });
      } else if (object.type === 'commit') {
        queue.push({ oid: want, gen: 1 });
      } else {
        extras.push(want);
      }
    }
    while (queue.length > 0) {
      const item = queue.shift() as { oid: string; gen: number };
      if (commitGen.has(item.oid) && (commitGen.get(item.oid) as number) <= item.gen) continue;
      commitGen.set(item.oid, item.gen);
      const object = repo.objects.get(item.oid);
      if (object === undefined || object.type !== 'commit') continue;
      if (item.gen >= depth) {
        newShallow.push(item.oid);
        continue;
      }
      for (const parent of parseCommit(object.data).parents) queue.push({ oid: parent, gen: item.gen + 1 });
    }
    // Every included commit, plus its tree closure (never its parents).
    for (const oid of commitGen.keys()) {
      const object = repo.objects.get(oid);
      if (object === undefined) continue;
      result.set(oid, object);
      const tree = parseCommit(object.data).tree;
      closure(repo, [tree], undefined, result);
    }
    closure(repo, extras, undefined, result);
  } else {
    closure(repo, wants, undefined, result);
  }

  if (haves.length > 0) {
    const haveSet = new Set(clientShallow);
    const haveObjects = new Map<string, GitObject>();
    closure(repo, haves, haveSet, haveObjects);
    for (const oid of haveObjects.keys()) result.delete(oid);
  }

  const unshallow: string[] = [];
  if (depth !== undefined) {
    const boundary = new Set(newShallow);
    for (const oid of clientShallow) if (!boundary.has(oid)) unshallow.push(oid);
  }
  return { objects: result, shallow: newShallow, unshallow };
}

/** `POST /<owner>/<repo>.git/git-upload-pack`. */
export function handleUploadPack(_req: IncomingMessage, res: ServerResponse, repo: Repo, body: Buffer): void {
  const lines: Buffer[] = [];
  let offset = 0;
  while (offset < body.length) {
    const packet = readPacket(body, offset);
    offset = packet.next;
    if (packet.kind === 'data') lines.push(packet.payload);
    else if (packet.kind === 'flush' || packet.kind === 'delim') continue;
    else break;
  }
  const text = lines.map((line) => line.toString('utf8'));
  const first = text[0] ?? '';
  if (first.startsWith('command=ls-refs')) {
    handleLsRefs(res, repo, text);
    return;
  }
  if (first.startsWith('command=fetch') || text.some((line) => line.startsWith('want '))) {
    handleFetch(res, repo, text);
    return;
  }
  res.writeHead(400, { 'content-type': 'text/plain; charset=utf-8' });
  res.end('bad request\n');
}

function handleLsRefs(res: ServerResponse, repo: Repo, lines: readonly string[]): void {
  const prefixes: string[] = [];
  let peel = false;
  let symrefs = false;
  let unborn = false;
  for (const raw of lines) {
    const line = raw.replace(/\n$/, '');
    if (line === 'peel') peel = true;
    else if (line === 'symrefs') symrefs = true;
    else if (line === 'unborn') unborn = true;
    else if (line.startsWith('ref-prefix ')) prefixes.push(line.slice('ref-prefix '.length));
  }
  const keep = (name: string): boolean => prefixes.length === 0 || prefixes.some((prefix) => name.startsWith(prefix));
  const parts: Array<string | 'flush'> = [];
  const tip = headOid(repo);
  if (keep('HEAD')) {
    if (tip === undefined) {
      if (unborn) parts.push(`unborn HEAD${symrefs ? ` symref-target:refs/heads/${repo.defaultBranch}` : ''}\n`);
    } else {
      parts.push(`${tip} HEAD${symrefs ? ` symref-target:refs/heads/${repo.defaultBranch}` : ''}\n`);
    }
  }
  for (const [ref, oid] of sortedRefs(repo)) {
    if (!keep(ref)) continue;
    let line = `${oid} ${ref}`;
    if (peel) {
      const peeled = peelOid(repo, oid);
      if (peeled !== undefined) line += ` peeled:${peeled}`;
    }
    parts.push(`${line}\n`);
  }
  parts.push('flush');
  const out = cached(repo, `ls:${lines.join('\0')}`, () => pktLines(parts));
  res.writeHead(200, { 'content-type': UPLOAD_RESULT, 'content-length': out.length });
  res.end(out);
}

function handleFetch(res: ServerResponse, repo: Repo, lines: readonly string[]): void {
  const wants: string[] = [];
  const haves: string[] = [];
  const clientShallow: string[] = [];
  let depth: number | undefined;
  for (const raw of lines) {
    const line = raw.replace(/\n$/, '');
    if (line.startsWith('want ')) wants.push(line.slice(5).trim());
    else if (line.startsWith('have ')) haves.push(line.slice(5).trim());
    else if (line.startsWith('shallow ')) clientShallow.push(line.slice(8).trim());
    else if (line.startsWith('deepen ')) {
      const value = Number.parseInt(line.slice(7).trim(), 10);
      if (Number.isFinite(value) && value > 0) depth = value;
    }
  }
  const out = cached(repo, `fetch:${lines.join('\0')}`, () => {
    const plan = planFetch(repo, wants, haves, depth, clientShallow);
    const pack = writePack(plan.objects.values());
    const chunks: Buffer[] = [];
    if (wants.length > 0) {
      chunks.push(pkt('acknowledgments\n'));
      chunks.push(pkt('NAK\n'));
      chunks.push(FLUSH);
      if (depth !== undefined && (plan.shallow.length > 0 || plan.unshallow.length > 0)) {
        const info: Array<string | 'flush'> = ['shallow-info\n'];
        for (const oid of plan.shallow) info.push(`shallow ${oid}\n`);
        for (const oid of plan.unshallow) info.push(`unshallow ${oid}\n`);
        info.push('flush');
        chunks.push(pktLines(info));
      }
      chunks.push(pkt('packfile\n'));
      chunks.push(sideband(pack));
      chunks.push(FLUSH);
    } else {
      chunks.push(FLUSH);
    }
    return Buffer.concat(chunks);
  });
  res.writeHead(200, { 'content-type': UPLOAD_RESULT, 'content-length': out.length });
  res.end(out);
}

interface PushCommand {
  oldOid: string;
  newOid: string;
  ref: string;
}

/** `POST /<owner>/<repo>.git/git-receive-pack`: unpack, update refs and report per ref. */
export function handleReceivePack(_req: IncomingMessage, res: ServerResponse, repo: Repo, body: Buffer): void {
  const commands: PushCommand[] = [];
  const caps = new Set<string>();
  let offset = 0;
  let malformed = false;
  while (offset < body.length) {
    const packet = readPacket(body, offset);
    if (packet.kind === 'bad') {
      malformed = true;
      offset = body.length;
      break;
    }
    offset = packet.next;
    if (packet.kind === 'flush') break;
    if (packet.kind !== 'data') continue;
    let line = packet.payload.toString('utf8').replace(/\n$/, '');
    const nul = line.indexOf('\u0000');
    if (nul >= 0) {
      for (const token of line.slice(nul + 1).split(' ')) if (token !== '') caps.add(token.split('=')[0]);
      line = line.slice(0, nul);
    }
    const parts = line.split(' ');
    if (parts.length >= 3) commands.push({ oldOid: parts[0], newOid: parts[1], ref: parts[2] });
  }
  const pack = offset < body.length ? body.subarray(offset) : Buffer.alloc(0);
  if (malformed) {
    res.writeHead(400, { 'content-type': 'text/plain; charset=utf-8' });
    res.end('bad pkt-line\n');
    return;
  }

  let unpackOk = true;
  if (pack.length > 0) {
    if (pack.length >= 12 && pack.toString('ascii', 0, 4) === 'PACK') {
      const read = readPack(pack);
      if (read.entries.length !== read.declaredCount) unpackOk = false;
      for (const object of read.objects.values()) putObject(repo, object.type, object.data);
    } else {
      unpackOk = false;
    }
  }

  const reports: Array<{ ref: string; ok: boolean; reason: string }> = [];
  for (const command of commands) {
    const current = repo.refs.get(command.ref);
    if (command.newOid === ZERO_OID) {
      if (current === undefined) reports.push({ ref: command.ref, ok: false, reason: 'ref does not exist' });
      else {
        repo.refs.delete(command.ref);
        reports.push({ ref: command.ref, ok: true, reason: '' });
      }
      continue;
    }
    if (!repo.objects.has(command.newOid)) {
      reports.push({ ref: command.ref, ok: false, reason: 'missing necessary objects' });
      continue;
    }
    if (current === undefined) {
      repo.refs.set(command.ref, command.newOid);
      reports.push({ ref: command.ref, ok: true, reason: '' });
      continue;
    }
    if (current === command.newOid) {
      reports.push({ ref: command.ref, ok: true, reason: '' });
      continue;
    }
    const oldCommit = commitOf(repo, current);
    const newCommit = command.newOid;
    if (oldCommit !== undefined && newCommit !== undefined && isAncestorFast(repo, oldCommit, newCommit)) {
      repo.refs.set(command.ref, command.newOid);
      reports.push({ ref: command.ref, ok: true, reason: '' });
    } else {
      reports.push({ ref: command.ref, ok: false, reason: 'non-fast-forward' });
    }
  }
  repo.updatedAt = new Date().toISOString();

  const inner: Array<string | 'flush'> = [`unpack ${unpackOk ? 'ok' : 'error'}\n`];
  for (const report of reports) inner.push(report.ok ? `ok ${report.ref}\n` : `ng ${report.ref} ${report.reason}\n`);
  inner.push('flush');
  const reportBytes = pktLines(inner);
  let out: Buffer;
  if (caps.has('side-band-64k')) {
    out = Buffer.concat([sideband(reportBytes), FLUSH]);
  } else {
    out = reportBytes;
  }
  res.writeHead(200, { 'content-type': RECEIVE_RESULT, 'content-length': out.length });
  res.end(out);
}

/** Ancestry walk used by fast-forward checks; kept separate so the hot push path stays compact. */
function isAncestorFast(repo: Repo, ancestor: string, descendant: string): boolean {
  if (ancestor === descendant) return true;
  const seen = new Set<string>();
  const stack = [descendant];
  while (stack.length > 0) {
    const oid = stack.pop() as string;
    if (seen.has(oid)) continue;
    seen.add(oid);
    const object = repo.objects.get(oid);
    if (object === undefined || object.type !== 'commit') continue;
    for (const parent of parseCommit(object.data).parents) {
      if (parent === ancestor) return true;
      if (!seen.has(parent)) stack.push(parent);
    }
  }
  return false;
}
