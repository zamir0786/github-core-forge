// The in-memory forge: users, repositories, git objects and refs, issues, comments, labels and
// pull requests. Every surface (REST, GraphQL, MCP and git) reads and writes this one store.

import {
  buildCommit,
  buildTreeFromFlat,
  flattenTree,
  hashObject,
  parseCommit,
  parseTree,
  parseTag,
  type FlatEntry,
  type GitObject,
} from './objects.ts';
import { iso, tick, ZERO_OID } from './util.ts';

export interface User {
  id: number;
  login: string;
  name: string;
  email: string;
  type: string;
}

export const VIEWER: User = { id: 1000, login: 'adaiversen', name: 'ada iversen', email: 'adaiversen@example.invalid', type: 'User' };
export const TEAMMATE: User = { id: 1001, login: 'quinnkeller', name: 'quinn keller', email: 'quinnkeller@example.invalid', type: 'User' };
export const STRANGER: User = { id: 1002, login: 'olgabauer', name: 'olga bauer', email: 'olgabauer@example.invalid', type: 'User' };

export interface Label {
  id: number;
  name: string;
  color: string;
  description: string;
}

export interface Issue {
  id: number;
  number: number;
  title: string;
  body: string;
  state: 'open' | 'closed';
  labels: Label[];
  created_at: string;
  updated_at: string;
  comments: number;
  cache?: string;
}

export interface Comment {
  id: number;
  body: string;
  created_at: string;
  updated_at: string;
  user: User;
}

export interface Pull {
  id: number;
  number: number;
  title: string;
  body: string;
  state: 'open' | 'closed';
  merged: boolean;
  merged_at: string | null;
  merge_commit_sha: string | null;
  headRef: string;
  baseRef: string;
  headSha: string;
  baseSha: string;
  created_at: string;
  updated_at: string;
  user: User;
}

export interface Repo {
  id: number;
  owner: string;
  name: string;
  description: string;
  private: boolean;
  defaultBranch: string;
  createdAt: string;
  updatedAt: string;
  objects: Map<string, GitObject>;
  refs: Map<string, string>;
  labels: Map<string, Label>;
  issues: Issue[];
  issueSeq: number;
  pulls: Pull[];
  pullSeq: number;
  comments: Map<number, Comment[]>;
}

export interface Store {
  repos: Map<string, Repo>;
  commentSeq: number;
  repoSeq: number;
  issueIdSeq: number;
  pullIdSeq: number;
  labelSeq: number;
  requestSeq: number;
}

export const store: Store = {
  repos: new Map(),
  commentSeq: 1000,
  repoSeq: 5000,
  issueIdSeq: 200000,
  pullIdSeq: 300000,
  labelSeq: 1,
  requestSeq: 0,
};

export function repoKey(owner: string, name: string): string {
  return `${owner.toLowerCase()}/${name.toLowerCase()}`;
}

export function findRepo(owner: string, name: string): Repo | undefined {
  return store.repos.get(repoKey(owner, name));
}

export function userOf(login: string): User {
  if (login.toLowerCase() === VIEWER.login) return VIEWER;
  if (login.toLowerCase() === TEAMMATE.login) return TEAMMATE;
  if (login.toLowerCase() === STRANGER.login) return STRANGER;
  return { id: 1003, login, name: login, email: `${login}@example.invalid`, type: 'User' };
}

export function createRepo(owner: string, name: string, description: string, isPrivate: boolean): Repo {
  store.repoSeq += 1;
  const now = iso(tick());
  const repo: Repo = {
    id: store.repoSeq,
    owner,
    name,
    description,
    private: isPrivate,
    defaultBranch: 'main',
    createdAt: now,
    updatedAt: now,
    objects: new Map(),
    refs: new Map(),
    labels: new Map(),
    issues: [],
    issueSeq: 0,
    pulls: [],
    pullSeq: 0,
    comments: new Map(),
  };
  store.repos.set(repoKey(owner, name), repo);
  return repo;
}

export function deleteRepo(repo: Repo): void {
  store.repos.delete(repoKey(repo.owner, repo.name));
}

export function repoJson(repo: Repo): Record<string, unknown> {
  return {
    id: repo.id,
    node_id: `R_${repo.id}`,
    name: repo.name,
    full_name: `${repo.owner}/${repo.name}`,
    private: repo.private,
    owner: { login: repo.owner, id: userOf(repo.owner).id, type: 'User' },
    description: repo.description,
    default_branch: repo.defaultBranch,
    created_at: repo.createdAt,
    updated_at: repo.updatedAt,
    pushed_at: repo.updatedAt,
    has_issues: true,
    has_wiki: false,
    has_projects: false,
    open_issues_count: repo.issues.filter((issue) => issue.state === 'open').length,
    forks_count: 0,
    stargazers_count: 0,
    watchers_count: 0,
    language: null,
    archived: false,
    disabled: false,
    visibility: repo.private ? 'private' : 'public',
    url: `/api/v3/repos/${repo.owner}/${repo.name}`,
    html_url: `http://localhost/${repo.owner}/${repo.name}`,
  };
}

// ---------------------------------------------------------------- git objects and refs

export function putObject(repo: Repo, type: GitObject['type'], data: Buffer): string {
  const oid = hashObject(type, data);
  if (!repo.objects.has(oid)) repo.objects.set(oid, { type, data });
  return oid;
}

export function objectAt(repo: Repo, oid: string): GitObject | undefined {
  return repo.objects.get(oid);
}

export function refOid(repo: Repo, ref: string): string | undefined {
  return repo.refs.get(ref);
}

export function defaultTip(repo: Repo): string | undefined {
  return repo.refs.get(`refs/heads/${repo.defaultBranch}`);
}

/** Peeling a ref to its commit, one tag hop at a time. */
export function commitOf(repo: Repo, oid: string): string | undefined {
  let current = oid;
  for (let hop = 0; hop < 8; hop += 1) {
    const object = repo.objects.get(current);
    if (object === undefined) return undefined;
    if (object.type === 'commit') return current;
    if (object.type === 'tag') current = parseTag(object.data).object;
    else return undefined;
  }
  return undefined;
}

export function commitTree(repo: Repo, commitOid: string): string | undefined {
  const object = repo.objects.get(commitOid);
  if (object === undefined || object.type !== 'commit') return undefined;
  return parseCommit(object.data).tree;
}

export function filesAtCommit(repo: Repo, commitOid: string | undefined): Map<string, FlatEntry> {
  if (commitOid === undefined) return new Map();
  const tree = commitTree(repo, commitOid);
  if (tree === undefined) return new Map();
  return flattenTree(repo.objects, tree);
}

export function filesAtRef(repo: Repo, ref: string): Map<string, FlatEntry> {
  const oid = repo.refs.get(ref.startsWith('refs/') ? ref : `refs/heads/${ref}`);
  const commit = oid === undefined ? undefined : commitOf(repo, oid);
  return filesAtCommit(repo, commit);
}

export function makeCommit(
  repo: Repo,
  files: ReadonlyMap<string, FlatEntry>,
  parents: readonly string[],
  message: string,
  author: User = VIEWER,
): string {
  const tree = buildTreeFromFlat(repo.objects, files);
  const stamp = tick();
  const data = buildCommit({
    tree,
    parents,
    author: `${author.name} <${author.email}> ${Math.floor(stamp / 1000)} +0000`,
    committer: `${author.name} <${author.email}> ${Math.floor(stamp / 1000)} +0000`,
    message,
  });
  return putObject(repo, 'commit', data);
}

export function isAncestor(repo: Repo, ancestor: string, descendant: string): boolean {
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

export function commitAncestors(repo: Repo, start: string, stopAt?: ReadonlySet<string>): Set<string> {
  const seen = new Set<string>();
  const stack = [start];
  while (stack.length > 0) {
    const oid = stack.pop() as string;
    if (seen.has(oid)) continue;
    seen.add(oid);
    const object = repo.objects.get(oid);
    if (object === undefined || object.type !== 'commit') continue;
    if (stopAt !== undefined && stopAt.has(oid)) continue;
    for (const parent of parseCommit(object.data).parents) if (!seen.has(parent)) stack.push(parent);
  }
  return seen;
}

/** Commit log from a tip, newest first, first-parent then the rest, capped. */
export function commitLog(repo: Repo, tip: string | undefined, limit = 100): Array<{ sha: string; view: ReturnType<typeof parseCommit> }> {
  if (tip === undefined) return [];
  const out: Array<{ sha: string; view: ReturnType<typeof parseCommit> }> = [];
  const seen = new Set<string>();
  const stack = [tip];
  while (stack.length > 0 && out.length < limit) {
    const oid = stack.shift() as string;
    if (seen.has(oid)) continue;
    seen.add(oid);
    const object = repo.objects.get(oid);
    if (object === undefined || object.type !== 'commit') continue;
    const view = parseCommit(object.data);
    out.push({ sha: oid, view });
    for (const parent of view.parents) if (!seen.has(parent)) stack.push(parent);
  }
  return out;
}

// ---------------------------------------------------------------- issues, labels, comments

export function createIssue(repo: Repo, title: string, body: string, labels?: readonly Label[]): Issue {
  repo.issueSeq += 1;
  store.issueIdSeq += 1;
  const now = iso(tick());
  const issue: Issue = {
    id: store.issueIdSeq,
    number: repo.issueSeq,
    title,
    body,
    state: 'open',
    labels: labels === undefined ? [] : [...labels],
    created_at: now,
    updated_at: now,
    comments: 0,
  };
  repo.issues.push(issue);
  return issue;
}

export function issueJson(issue: Issue, repo: Repo): Record<string, unknown> {
  return {
    id: issue.id,
    node_id: `I_${issue.id}`,
    number: issue.number,
    title: issue.title,
    body: issue.body,
    state: issue.state,
    state_reason: issue.state === 'closed' ? 'completed' : null,
    locked: false,
    labels: issue.labels.map(labelJson),
    user: { login: VIEWER.login, id: VIEWER.id, type: 'User' },
    comments: issue.comments,
    created_at: issue.created_at,
    updated_at: issue.updated_at,
    closed_at: issue.state === 'closed' ? issue.updated_at : null,
    html_url: `http://localhost/${repo.owner}/${repo.name}/issues/${issue.number}`,
    url: `/api/v3/repos/${repo.owner}/${repo.name}/issues/${issue.number}`,
    repository_url: `/api/v3/repos/${repo.owner}/${repo.name}`,
  };
}

/** Serialize an issue directly to its REST shape; avoids the object + stringify round trip. */
export function issueJsonText(issue: Issue, repo: Repo): string {
  const q = JSON.stringify;
  let labels = '[';
  for (let i = 0; i < issue.labels.length; i += 1) {
    const label = issue.labels[i] as Label;
    if (i > 0) labels += ',';
    labels += `{"id":${label.id},"name":${q(label.name)},"color":${q(label.color)},"description":${q(label.description)},"default":false}`;
  }
  labels += ']';
  const closed = issue.state === 'closed';
  return `{"id":${issue.id},"node_id":${q(`I_${issue.id}`)},"number":${issue.number},"title":${q(issue.title)},"body":${q(issue.body)},"state":${q(issue.state)},"state_reason":${closed ? '"completed"' : 'null'},"locked":false,"labels":${labels},"user":{"login":${q(VIEWER.login)},"id":${VIEWER.id},"type":"User"},"comments":${issue.comments},"created_at":${q(issue.created_at)},"updated_at":${q(issue.updated_at)},"closed_at":${closed ? q(issue.updated_at) : 'null'},"html_url":${q(`http://localhost/${repo.owner}/${repo.name}/issues/${issue.number}`)},"url":${q(`/api/v3/repos/${repo.owner}/${repo.name}/issues/${issue.number}`)},"repository_url":${q(`/api/v3/repos/${repo.owner}/${repo.name}`)}}`;
}

export function issueString(issue: Issue, repo: Repo): string {
  if (issue.cache !== undefined) return issue.cache;
  issue.cache = `${issueJsonText(issue, repo)}\n`;
  return issue.cache;
}

export function labelJson(label: Label): Record<string, unknown> {
  return { id: label.id, name: label.name, color: label.color, description: label.description, default: false };
}

export function addComment(repo: Repo, issue: Issue, body: string): Comment {
  store.commentSeq += 1;
  const now = iso(tick());
  const comment: Comment = { id: store.commentSeq, body, created_at: now, updated_at: now, user: VIEWER };
  const list = repo.comments.get(issue.id) ?? [];
  list.push(comment);
  repo.comments.set(issue.id, list);
  issue.comments = list.length;
  issue.updated_at = now;
  issue.cache = undefined;
  return comment;
}

export function commentJson(comment: Comment): Record<string, unknown> {
  return {
    id: comment.id,
    node_id: `IC_${comment.id}`,
    body: comment.body,
    user: { login: comment.user.login, id: comment.user.id, type: 'User' },
    created_at: comment.created_at,
    updated_at: comment.updated_at,
    html_url: '',
  };
}

// ---------------------------------------------------------------- pull requests

export function createPull(repo: Repo, input: { title: string; body: string; head: string; base: string }): Pull | string {
  const headOid = repo.refs.get(`refs/heads/${input.head}`);
  const baseOid = repo.refs.get(`refs/heads/${input.base}`);
  if (headOid === undefined) return `No commits between ${input.base} and ${input.head}`;
  if (baseOid === undefined) return `Base branch ${input.base} does not exist`;
  repo.pullSeq += 1;
  store.pullIdSeq += 1;
  const now = iso(tick());
  const pull: Pull = {
    id: store.pullIdSeq,
    number: repo.pullSeq,
    title: input.title,
    body: input.body,
    state: 'open',
    merged: false,
    merged_at: null,
    merge_commit_sha: null,
    headRef: input.head,
    baseRef: input.base,
    headSha: headOid,
    baseSha: baseOid,
    created_at: now,
    updated_at: now,
    user: VIEWER,
  };
  repo.pulls.push(pull);
  return pull;
}

export function pullJson(pull: Pull, repo: Repo): Record<string, unknown> {
  return {
    id: pull.id,
    node_id: `PR_${pull.id}`,
    number: pull.number,
    title: pull.title,
    body: pull.body,
    state: pull.state,
    merged: pull.merged,
    merged_at: pull.merged_at,
    merge_commit_sha: pull.merge_commit_sha,
    head: { ref: pull.headRef, sha: pull.headSha, label: `${repo.owner}:${pull.headRef}` },
    base: { ref: pull.baseRef, sha: pull.baseSha, label: `${repo.owner}:${pull.baseRef}` },
    user: { login: VIEWER.login, id: VIEWER.id, type: 'User' },
    created_at: pull.created_at,
    updated_at: pull.updated_at,
    draft: false,
    html_url: `http://localhost/${repo.owner}/${repo.name}/pull/${pull.number}`,
    url: `/api/v3/repos/${repo.owner}/${repo.name}/pulls/${pull.number}`,
  };
}

export interface DiffFile {
  filename: string;
  status: 'added' | 'modified' | 'removed';
  additions: number;
  deletions: number;
  changes: number;
  sha: string;
}

export function diffFiles(repo: Repo, baseCommit: string | undefined, headCommit: string | undefined): DiffFile[] {
  const base = filesAtCommit(repo, baseCommit);
  const head = filesAtCommit(repo, headCommit);
  const out: DiffFile[] = [];
  for (const [path, entry] of head) {
    const before = base.get(path);
    if (before !== undefined && before.oid === entry.oid) continue;
    const added = before === undefined;
    out.push({
      filename: path,
      status: added ? 'added' : 'modified',
      additions: 1,
      deletions: added ? 0 : 1,
      changes: added ? 1 : 2,
      sha: entry.oid,
    });
  }
  for (const [path, entry] of base) {
    if (head.has(path)) continue;
    out.push({ filename: path, status: 'removed', additions: 0, deletions: 1, changes: 1, sha: entry.oid });
  }
  return out.sort((a, b) => (a.filename < b.filename ? -1 : a.filename > b.filename ? 1 : 0));
}

export function mergeBase(repo: Repo, a: string, b: string): string | undefined {
  const ancestors = commitAncestors(repo, a);
  const seen = new Set<string>();
  const stack = [b];
  while (stack.length > 0) {
    const oid = stack.shift() as string;
    if (seen.has(oid)) continue;
    seen.add(oid);
    if (ancestors.has(oid)) return oid;
    const object = repo.objects.get(oid);
    if (object === undefined || object.type !== 'commit') continue;
    for (const parent of parseCommit(object.data).parents) stack.push(parent);
  }
  return undefined;
}

export type MergeResult = { ok: true; sha: string; pull: Pull } | { ok: false; status: number; message: string };

/** Squash-merge `pull` into its base branch, refusing a real content conflict with 405. */
export function mergePull(repo: Repo, pull: Pull): MergeResult {
  if (pull.merged) return { ok: false, status: 405, message: 'Pull request is not mergeable' };
  const baseTip = repo.refs.get(`refs/heads/${pull.baseRef}`);
  const headTip = repo.refs.get(`refs/heads/${pull.headRef}`);
  if (baseTip === undefined || headTip === undefined) return { ok: false, status: 405, message: 'Pull request is not mergeable' };
  const baseCommit = commitOf(repo, baseTip);
  const headCommit = commitOf(repo, headTip);
  if (baseCommit === undefined || headCommit === undefined) return { ok: false, status: 405, message: 'Pull request is not mergeable' };
  const common = mergeBase(repo, baseCommit, headCommit);
  const baseFiles = filesAtCommit(repo, baseCommit);
  const headFiles = filesAtCommit(repo, headCommit);
  const commonFiles = filesAtCommit(repo, common);
  const result = new Map(baseFiles);
  for (const [path, entry] of headFiles) {
    const before = commonFiles.get(path);
    if (before !== undefined && before.oid === entry.oid) continue;
    const current = baseFiles.get(path);
    if (current !== undefined && current.oid !== entry.oid && before !== undefined && before.oid !== current.oid) {
      return { ok: false, status: 405, message: 'Pull request has merge conflicts' };
    }
    result.set(path, entry);
  }
  for (const path of commonFiles.keys()) {
    if (!headFiles.has(path) && baseFiles.get(path)?.oid === commonFiles.get(path)?.oid) result.delete(path);
  }
  const sha = makeCommit(repo, result, [baseTip], `Merge pull request #${pull.number} from ${pull.headRef}\n\n${pull.title}`);
  repo.refs.set(`refs/heads/${pull.baseRef}`, sha);
  pull.state = 'closed';
  pull.merged = true;
  pull.merged_at = iso(tick());
  pull.merge_commit_sha = sha;
  pull.updated_at = pull.merged_at;
  return { ok: true, sha, pull };
}
