// The GitHub REST v3 surface: repositories, branches, commits, contents, issues, labels and
// pull requests. Every response is JSON and carries an X-GitHub-Request-Id.

import type { IncomingMessage, ServerResponse } from 'node:http';
import { parseCommit, parseTree, type GitObject } from './objects.ts';
import {
  addComment,
  commentJson,
  commitLog,
  commitOf,
  createIssue,
  createPull,
  createRepo,
  deleteRepo,
  diffFiles,
  filesAtCommit,
  findRepo,
  issueJson,
  issueString,
  labelJson,
  makeCommit,
  mergePull,
  putObject,
  repoJson,
  store,
  VIEWER,
  type FlatEntry,
  type Issue,
  type Label,
  type Pull,
  type Repo,
} from './store.ts';
import { iso, JSON_TYPE, safeDecode, sendJson, tick } from './util.ts';

export interface RestContext {
  method: string;
  segments: string[];
  query: URLSearchParams;
  bodyText: string | null;
  host: string;
}

function headers(): Record<string, string> {
  store.requestSeq += 1;
  return { 'x-github-request-id': `MF-${store.requestSeq.toString(16).padStart(8, '0')}` };
}

function fail(res: ServerResponse, status: number, message: string): void {
  sendJson(res, status, { message, documentation_url: 'https://docs.github.com/rest' }, headers());
}

type Parsed = { ok: true; value: Record<string, unknown> } | { ok: false };

function parseJsonBody(context: RestContext): Parsed {
  const text = context.bodyText ?? '';
  if (text.trim() === '') return { ok: true, value: {} };
  try {
    const value = JSON.parse(text) as unknown;
    if (value === null || typeof value !== 'object' || Array.isArray(value)) return { ok: false };
    return { ok: true, value: value as Record<string, unknown> };
  } catch {
    return { ok: false };
  }
}

function str(value: unknown, fallback = ''): string {
  return typeof value === 'string' ? value : fallback;
}

function bool(value: unknown, fallback = false): boolean {
  return typeof value === 'boolean' ? value : fallback;
}

function perPageOf(context: RestContext): number {
  const raw = Number.parseInt(context.query.get('per_page') ?? '30', 10);
  return Number.isFinite(raw) && raw > 0 ? Math.min(100, raw) : 30;
}

function pageOf(context: RestContext): number {
  const raw = Number.parseInt(context.query.get('page') ?? '1', 10);
  return Number.isFinite(raw) && raw > 0 ? raw : 1;
}

function linkHeader(host: string, path: string, context: RestContext, page: number, perPage: number, total: number): string {
  const base = new URLSearchParams(context.query.toString());
  base.set('per_page', String(perPage));
  const urlFor = (target: number): string => {
    const params = new URLSearchParams(base);
    params.set('page', String(target));
    return `http://${host}${path}?${params.toString()}`;
  };
  const last = Math.max(1, Math.ceil(total / perPage));
  const links: string[] = [];
  if (page < last) links.push(`<${urlFor(page + 1)}>; rel="next"`);
  if (page > 1) links.push(`<${urlFor(page - 1)}>; rel="prev"`);
  links.push(`<${urlFor(1)}>; rel="first"`);
  links.push(`<${urlFor(last)}>; rel="last"`);
  return links.join(', ');
}

/** The route table. Always returns true: /api/v3 is this server's. */
export function handleRest(req: IncomingMessage, res: ServerResponse, context: RestContext): boolean {
  const { method, segments } = context;
  const first = segments[0] ?? '';

  if (first === '') {
    sendJson(res, 200, {
      current_user_url: '/api/v3/user',
      authorizations_url: '/api/v3/authorizations',
      emails_url: '/api/v3/user/emails',
      feeds_url: '/api/v3/feeds',
      followers_url: '/api/v3/user/followers',
      following_url: '/api/v3/user/following{/target}',
      gists_url: '/api/v3/gists{/gist_id}',
      issues_url: '/api/v3/issues',
      notifications_url: '/api/v3/notifications',
      organization_url: '/api/v3/orgs/{org}',
      organizations_url: '/api/v3/user/orgs',
      rate_limit_url: '/api/v3/rate_limit',
      repository_url: '/api/v3/repos/{owner}/{repo}',
      current_user_repositories_url: '/api/v3/user/repos{?type,page,per_page,sort}',
      user_url: '/api/v3/users/{user}',
      user_repositories_url: '/api/v3/users/{user}/repos{?type,page,per_page,sort}',
    }, { ...headers(), 'x-oauth-scopes': 'repo, read:org, gist, workflow' });
    return true;
  }

  if (first === 'meta') {
    sendJson(res, 200, {
      installed_version: '3.0.0-makefun',
      verifiable_password_authentication: true,
      ssh_key_fingerprints: {},
      hooks: ['127.0.0.1/32'],
    }, headers());
    return true;
  }

  if (first === 'user') {
    if (segments.length === 1) {
      if (method !== 'GET') {
        fail(res, 404, 'Not Found');
        return true;
      }
      sendJson(res, 200, {
        login: VIEWER.login,
        id: VIEWER.id,
        node_id: `U_${VIEWER.id}`,
        name: VIEWER.name,
        email: VIEWER.email,
        type: 'User',
        site_admin: false,
        public_repos: 0,
        followers: 0,
        following: 0,
        created_at: iso(0),
        updated_at: iso(0),
      }, headers());
      return true;
    }
    if (segments[1] === 'repos') {
      if (method === 'GET') return listOwnRepos(res, context);
      if (method === 'POST') return createRepoRoute(res, context);
    }
    fail(res, 404, 'Not Found');
    return true;
  }

  if (first === 'repos' && segments.length >= 3) return handleRepoRoute(req, res, context);

  fail(res, 404, 'Not Found');
  return true;
}

function listOwnRepos(res: ServerResponse, context: RestContext): boolean {
  const perPage = perPageOf(context);
  const page = pageOf(context);
  const all = [...store.repos.values()]
    .filter((repo) => repo.owner.toLowerCase() === VIEWER.login)
    .sort((a, b) => a.id - b.id);
  const slice = all.slice((page - 1) * perPage, page * perPage);
  const outHeaders = headers();
  outHeaders.link = linkHeader(context.host, '/api/v3/user/repos', context, page, perPage, all.length);
  sendJson(res, 200, slice.map(repoJson), outHeaders);
  return true;
}

function createRepoRoute(res: ServerResponse, context: RestContext): boolean {
  const parsed = parseJsonBody(context);
  if (!parsed.ok) {
    fail(res, 400, 'Problems parsing JSON');
    return true;
  }
  const name = str(parsed.value.name).trim();
  if (name === '') {
    fail(res, 422, 'Repository creation failed. Name is required');
    return true;
  }
  if (findRepo(VIEWER.login, name) !== undefined) {
    fail(res, 422, `Repository creation failed. Repository ${VIEWER.login}/${name} already exists`);
    return true;
  }
  const repo = createRepo(VIEWER.login, name, str(parsed.value.description), bool(parsed.value.private));
  if (typeof parsed.value.default_branch === 'string' && parsed.value.default_branch !== '') {
    repo.defaultBranch = parsed.value.default_branch;
  }
  sendJson(res, 201, repoJson(repo), {
    ...headers(),
    location: `http://${context.host}/api/v3/repos/${VIEWER.login}/${name}`,
  });
  return true;
}

function handleRepoRoute(req: IncomingMessage, res: ServerResponse, context: RestContext): boolean {
  const { method, segments } = context;
  const repo = findRepo(segments[1] ?? '', segments[2] ?? '');
  if (repo === undefined) {
    fail(res, 404, 'Not Found');
    return true;
  }
  if (repo.owner.toLowerCase() !== VIEWER.login) {
    fail(res, 404, 'Not Found');
    return true;
  }

  if (segments.length === 3) {
    if (method === 'GET') {
      sendJson(res, 200, repoJson(repo), headers());
      return true;
    }
    if (method === 'PATCH') {
      const parsed = parseJsonBody(context);
      if (!parsed.ok) {
        fail(res, 400, 'Problems parsing JSON');
        return true;
      }
      if (typeof parsed.value.description === 'string') repo.description = parsed.value.description;
      if (typeof parsed.value.name === 'string' && parsed.value.name !== '' && parsed.value.name !== repo.name) {
        store.repos.delete(`${repo.owner}/${repo.name}`.toLowerCase());
        repo.name = parsed.value.name;
        store.repos.set(`${repo.owner}/${repo.name}`.toLowerCase(), repo);
      }
      if (typeof parsed.value.private === 'boolean') repo.private = parsed.value.private;
      repo.updatedAt = iso(tick());
      sendJson(res, 200, repoJson(repo), headers());
      return true;
    }
    if (method === 'DELETE') {
      deleteRepo(repo);
      res.writeHead(204, headers());
      res.end();
      return true;
    }
    fail(res, 404, 'Not Found');
    return true;
  }

  const resource = segments[3] ?? '';
  if (resource === 'labels') return handleLabels(res, context, repo);
  if (resource === 'issues') return handleIssues(req, res, context, repo);
  if (resource === 'pulls') return handlePulls(req, res, context, repo);
  if (resource === 'branches') return handleBranches(res, context, repo);
  if (resource === 'commits') return handleCommits(res, context, repo);
  if (resource === 'contents') return handleContents(req, res, context, repo);
  if (resource === 'git') return handleGitData(res, context, repo);
  fail(res, 404, 'Not Found');
  return true;
}

function handleLabels(res: ServerResponse, context: RestContext, repo: Repo): boolean {
  if (context.method === 'GET') {
    sendJson(res, 200, [...repo.labels.values()].map(labelJson), headers());
    return true;
  }
  if (context.method !== 'POST') {
    fail(res, 404, 'Not Found');
    return true;
  }
  const parsed = parseJsonBody(context);
  if (!parsed.ok) {
    fail(res, 400, 'Problems parsing JSON');
    return true;
  }
  const name = str(parsed.value.name).trim();
  if (name === '') {
    fail(res, 422, 'Validation Failed');
    return true;
  }
  const existing = repo.labels.get(name.toLowerCase());
  if (existing !== undefined) {
    sendJson(res, 201, labelJson(existing), headers());
    return true;
  }
  store.labelSeq += 1;
  const label: Label = {
    id: store.labelSeq,
    name,
    color: str(parsed.value.color, 'ededed'),
    description: str(parsed.value.description),
  };
  repo.labels.set(name.toLowerCase(), label);
  sendJson(res, 201, labelJson(label), headers());
  return true;
}

function handleBranches(res: ServerResponse, context: RestContext, repo: Repo): boolean {
  const rest = context.segments.slice(4).map(safeDecode);
  if (rest.length === 0) {
    const out = [...repo.refs.entries()]
      .filter(([ref]) => ref.startsWith('refs/heads/'))
      .sort((a, b) => (a[0] < b[0] ? -1 : 1))
      .map(([ref, oid]) => ({
        name: ref.slice('refs/heads/'.length),
        commit: { sha: oid, url: '' },
        protected: false,
      }));
    sendJson(res, 200, out, headers());
    return true;
  }
  const branch = rest.join('/');
  const oid = repo.refs.get(`refs/heads/${branch}`);
  if (oid === undefined) {
    fail(res, 404, 'Branch not found');
    return true;
  }
  sendJson(res, 200, { name: branch, commit: { sha: oid, url: '' }, protected: false }, headers());
  return true;
}

function handleCommits(res: ServerResponse, context: RestContext, repo: Repo): boolean {
  if (context.method !== 'GET') {
    fail(res, 404, 'Not Found');
    return true;
  }
  const requested = context.query.get('sha') ?? context.query.get('ref') ?? repo.defaultBranch;
  const direct = repo.refs.get(requested.startsWith('refs/') ? requested : `refs/heads/${requested}`);
  const oid = direct ?? (repo.objects.has(requested) ? requested : undefined);
  const tip = oid === undefined ? undefined : commitOf(repo, oid);
  const perPage = perPageOf(context);
  const out = commitLog(repo, tip, Math.min(200, perPage)).map(({ sha, view }) => {
    const ident = (line: string): Record<string, string> => {
      const match = /^(.*) <([^>]*)> (\d+) ([+-]\d+)$/.exec(line);
      if (match === null) return { name: line, email: '', date: iso(0) };
      return {
        name: match[1] ?? '',
        email: match[2] ?? '',
        date: new Date(Number(match[3]) * 1000).toISOString().replace(/\.\d{3}Z$/, 'Z'),
      };
    };
    return {
      sha,
      node_id: `C_${sha}`,
      commit: {
        message: view.message.endsWith('\n') ? view.message.slice(0, -1) : view.message,
        author: ident(view.author),
        committer: ident(view.committer),
        tree: { sha: view.tree, url: '' },
      },
      parents: view.parents.map((parent) => ({ sha: parent, url: '' })),
      url: `/api/v3/repos/${repo.owner}/${repo.name}/commits/${sha}`,
    };
  });
  sendJson(res, 200, out, headers());
  return true;
}

function handleGitData(res: ServerResponse, context: RestContext, repo: Repo): boolean {
  const rest = context.segments.slice(4).map(safeDecode);
  if (rest[0] === 'refs' && rest.length >= 2) {
    if (rest[1] === 'heads' && rest.length >= 3) {
      const branch = rest.slice(2).join('/');
      const oid = repo.refs.get(`refs/heads/${branch}`);
      if (oid === undefined) {
        fail(res, 404, 'Not Found');
        return true;
      }
      sendJson(res, 200, {
        ref: `refs/heads/${branch}`,
        node_id: `R_${oid}`,
        url: '',
        object: { sha: oid, type: 'commit', url: '' },
      }, headers());
      return true;
    }
    if (rest[1] === 'tags' && rest.length >= 3) {
      const tag = rest.slice(2).join('/');
      const oid = repo.refs.get(`refs/tags/${tag}`);
      if (oid === undefined) {
        fail(res, 404, 'Not Found');
        return true;
      }
      sendJson(res, 200, {
        ref: `refs/tags/${tag}`,
        node_id: `R_${oid}`,
        url: '',
        object: { sha: oid, type: repo.objects.get(oid)?.type ?? 'tag', url: '' },
      }, headers());
      return true;
    }
    if (rest[1] === 'matching-refs' && rest.length >= 3) {
      const prefix = `refs/${rest.slice(2).join('/')}`;
      const out = [...repo.refs.entries()]
        .filter(([ref]) => ref.startsWith(prefix))
        .sort((a, b) => (a[0] < b[0] ? -1 : 1))
        .map(([ref, oid]) => ({ ref, node_id: `R_${oid}`, url: '', object: { sha: oid, type: 'commit', url: '' } }));
      sendJson(res, 200, out, headers());
      return true;
    }
  }
  if (rest[0] === 'refs' && context.method === 'POST') {
    const parsed = parseJsonBody(context);
    if (!parsed.ok) {
      fail(res, 400, 'Problems parsing JSON');
      return true;
    }
    const ref = str(parsed.value.ref);
    const sha = str(parsed.value.sha);
    if (ref === '' || !repo.objects.has(sha)) {
      fail(res, 422, 'Reference update failed');
      return true;
    }
    if (repo.refs.has(ref)) {
      fail(res, 422, 'Reference already exists');
      return true;
    }
    repo.refs.set(ref, sha);
    sendJson(res, 201, {
      ref,
      node_id: `R_${sha}`,
      url: '',
      object: { sha, type: repo.objects.get(sha)?.type ?? 'commit', url: '' },
    }, headers());
    return true;
  }
  if (rest[0] === 'refs' && rest.length >= 3 && (context.method === 'PATCH' || context.method === 'DELETE')) {
    const ref = `refs/${rest.slice(1).join('/')}`;
    if (context.method === 'DELETE') {
      if (!repo.refs.has(ref)) {
        fail(res, 422, 'Reference does not exist');
        return true;
      }
      repo.refs.delete(ref);
      res.writeHead(204, headers());
      res.end();
      return true;
    }
    repo.refs.delete(ref);
    return handleGitData(res, context, repo);
  }
  fail(res, 404, 'Not Found');
  return true;
}

function handleIssues(req: IncomingMessage, res: ServerResponse, context: RestContext, repo: Repo): boolean {
  const rest = context.segments.slice(4).map(safeDecode);
  if (rest.length === 0) {
    if (context.method === 'GET') {
      return listIssues(res, context, repo);
    }
    if (context.method === 'POST') {
      const parsed = parseJsonBody(context);
      if (!parsed.ok) {
        fail(res, 400, 'Problems parsing JSON');
        return true;
      }
      const title = str(parsed.value.title);
      if (title.trim() === '') {
        fail(res, 422, 'Validation Failed');
        return true;
      }
      const labels = resolveLabels(repo, parsed.value.labels);
      if (typeof labels === 'string') {
        fail(res, 422, `Validation Failed: ${labels}`);
        return true;
      }
      const issue = createIssue(repo, title, str(parsed.value.body), labels);
      sendJson(res, 201, issueJson(issue, repo), {
        ...headers(),
        location: `http://${context.host}/api/v3/repos/${repo.owner}/${repo.name}/issues/${issue.number}`,
      });
      return true;
    }
    fail(res, 404, 'Not Found');
    return true;
  }
  const number = Number.parseInt(rest[0] ?? '', 10);
  if (!Number.isFinite(number)) {
    fail(res, 404, 'Not Found');
    return true;
  }
  const issue = repo.issues.find((entry) => entry.number === number);
  if (issue === undefined) {
    fail(res, 404, 'Not Found');
    return true;
  }
  if (rest.length === 2 && rest[1] === 'comments') {
    if (context.method === 'GET') {
      sendJson(res, 200, (repo.comments.get(issue.id) ?? []).map(commentJson), headers());
      return true;
    }
    if (context.method === 'POST') {
      const parsed = parseJsonBody(context);
      if (!parsed.ok) {
        fail(res, 400, 'Problems parsing JSON');
        return true;
      }
      const comment = addComment(repo, issue, str(parsed.value.body));
      sendJson(res, 201, commentJson(comment), headers());
      return true;
    }
  }
  if (rest.length === 1) {
    if (context.method === 'GET') {
      const text = issueString(issue, repo);
      res.writeHead(200, { 'content-type': JSON_TYPE, 'content-length': Buffer.byteLength(text), ...headers() });
      res.end(text);
      return true;
    }
    if (context.method === 'PATCH') {
      const parsed = parseJsonBody(context);
      if (!parsed.ok) {
        fail(res, 400, 'Problems parsing JSON');
        return true;
      }
      if (parsed.value.state === 'open' || parsed.value.state === 'closed') issue.state = parsed.value.state;
      if (typeof parsed.value.title === 'string') issue.title = parsed.value.title;
      if (typeof parsed.value.body === 'string') issue.body = parsed.value.body;
      if (parsed.value.labels !== undefined) {
        const labels = resolveLabels(repo, parsed.value.labels);
        if (typeof labels === 'string') {
          fail(res, 422, `Validation Failed: ${labels}`);
          return true;
        }
        issue.labels = labels;
      }
      issue.updated_at = iso(tick());
      issue.cache = undefined;
      sendJson(res, 200, issueJson(issue, repo), headers());
      return true;
    }
  }
  fail(res, 404, 'Not Found');
  return true;
}

function resolveLabels(repo: Repo, value: unknown): Label[] | string {
  const labels: Label[] = [];
  if (!Array.isArray(value)) return labels;
  for (const name of value) {
    const label = repo.labels.get(String(name).toLowerCase());
    if (label === undefined) return `label ${String(name)} does not exist`;
    labels.push(label);
  }
  return labels;
}

function listIssues(res: ServerResponse, context: RestContext, repo: Repo): boolean {
  const state = context.query.get('state') ?? 'open';
  const sort = context.query.get('sort') ?? 'created';
  const direction = context.query.get('direction') ?? 'desc';
  const perPage = perPageOf(context);
  const page = pageOf(context);
  const labelsFilter = context.query.getAll('labels').flatMap((value) => value.split(',')).filter((value) => value !== '');
  let list = repo.issues.filter((issue) => state === 'all' || issue.state === state);
  if (labelsFilter.length > 0) {
    list = list.filter((issue) => labelsFilter.every((name) => issue.labels.some((label) => label.name.toLowerCase() === name.toLowerCase())));
  }
  if (sort === 'created' || sort === 'updated') {
    list = [...list].sort((a, b) => (direction === 'asc' ? a.number - b.number : b.number - a.number));
  }
  const total = list.length;
  const slice = list.slice((page - 1) * perPage, page * perPage);
  const outHeaders = headers();
  outHeaders.link = linkHeader(context.host, `/api/v3/repos/${repo.owner}/${repo.name}/issues`, context, page, perPage, total);
  sendJson(res, 200, slice.map((issue) => issueJson(issue, repo)), outHeaders);
  return true;
}

function handlePulls(req: IncomingMessage, res: ServerResponse, context: RestContext, repo: Repo): boolean {
  const rest = context.segments.slice(4).map(safeDecode);
  if (rest.length === 0) {
    if (context.method === 'GET') {
      const state = context.query.get('state') ?? 'open';
      const perPage = perPageOf(context);
      const page = pageOf(context);
      let list = repo.pulls.filter((pull) => state === 'all' || pull.state === state);
      const headFilter = context.query.get('head');
      if (headFilter !== null && headFilter !== '') {
        const branch = headFilter.split(':').pop() ?? headFilter;
        list = list.filter((pull) => pull.headRef === branch);
      }
      const total = list.length;
      const slice = list.slice((page - 1) * perPage, page * perPage);
      const outHeaders = headers();
      outHeaders.link = linkHeader(context.host, `/api/v3/repos/${repo.owner}/${repo.name}/pulls`, context, page, perPage, total);
      sendJson(res, 200, slice.map((pull) => pullJsonOf(pull, repo)), outHeaders);
      return true;
    }
    if (context.method === 'POST') {
      const parsed = parseJsonBody(context);
      if (!parsed.ok) {
        fail(res, 400, 'Problems parsing JSON');
        return true;
      }
      const head = str(parsed.value.head).split(':').pop() ?? '';
      const base = str(parsed.value.base).split(':').pop() ?? '';
      const created = createPull(repo, {
        title: str(parsed.value.title),
        body: str(parsed.value.body),
        head,
        base,
      });
      if (typeof created === 'string') {
        fail(res, 422, `Validation Failed: ${created}`);
        return true;
      }
      sendJson(res, 201, pullJsonOf(created, repo), {
        ...headers(),
        location: `http://${context.host}/api/v3/repos/${repo.owner}/${repo.name}/pulls/${created.number}`,
      });
      return true;
    }
    fail(res, 404, 'Not Found');
    return true;
  }
  const number = Number.parseInt(rest[0] ?? '', 10);
  const pull = repo.pulls.find((entry) => entry.number === number);
  if (pull === undefined) {
    fail(res, 404, 'Not Found');
    return true;
  }
  if (rest.length === 1) {
    if (context.method !== 'GET') {
      fail(res, 404, 'Not Found');
      return true;
    }
    sendJson(res, 200, pullJsonOf(pull, repo), headers());
    return true;
  }
  if (rest[1] === 'files') {
    const baseCommit = commitOf(repo, pull.baseSha);
    const headCommit = commitOf(repo, pull.headSha);
    const files = diffFiles(repo, baseCommit, headCommit).map((file) => ({
      sha: file.sha,
      filename: file.filename,
      status: file.status,
      additions: file.additions,
      deletions: file.deletions,
      changes: file.changes,
      blob_url: '',
      raw_url: '',
      contents_url: '',
    }));
    sendJson(res, 200, files, headers());
    return true;
  }
  if (rest[1] === 'merge' && context.method === 'PUT') {
    const result = mergePull(repo, pull);
    if (!result.ok) {
      fail(res, result.status, result.message);
      return true;
    }
    sendJson(res, 200, { sha: result.sha, merged: true, message: 'Pull Request successfully merged' }, headers());
    return true;
  }
  fail(res, 404, 'Not Found');
  return true;
}

function pullJsonOf(pull: Pull, repo: Repo): Record<string, unknown> {
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

function resolveCommit(repo: Repo, ref: string): string | undefined {
  const direct = repo.refs.get(ref.startsWith('refs/') ? ref : `refs/heads/${ref}`);
  if (direct !== undefined) return commitOf(repo, direct);
  if (repo.objects.has(ref)) return commitOf(repo, ref);
  return undefined;
}

function handleContents(req: IncomingMessage, res: ServerResponse, context: RestContext, repo: Repo): boolean {
  const rest = context.segments.slice(4).map(safeDecode);
  const path = rest.join('/');
  const ref = context.query.get('ref') ?? repo.defaultBranch;
  if (context.method === 'GET') {
    const commit = resolveCommit(repo, ref);
    if (commit === undefined) {
      fail(res, 404, 'Not Found');
      return true;
    }
    const commitObject = repo.objects.get(commit);
    if (commitObject === undefined) {
      fail(res, 404, 'Not Found');
      return true;
    }
    if (path === '') {
      const rootTree = repo.objects.get(parseCommit(commitObject.data).tree);
      if (rootTree === undefined) {
        fail(res, 404, 'Not Found');
        return true;
      }
      sendJson(res, 200, listingOf(repo, rootTree, ''), headers());
      return true;
    }
    const segments = path.split('/');
    let current: GitObject | undefined = repo.objects.get(parseCommit(commitObject.data).tree);
    for (let index = 0; index < segments.length; index += 1) {
      if (current === undefined) {
        fail(res, 404, 'Not Found');
        return true;
      }
      const entry = parseTree(current.data).find((candidate) => candidate.name === segments[index]);
      if (entry === undefined) {
        fail(res, 404, 'Not Found');
        return true;
      }
      const isLast = index === segments.length - 1;
      if (isLast) {
        if (entry.mode === '40000') {
          const dirTree = repo.objects.get(entry.oid);
          if (dirTree === undefined) {
            fail(res, 404, 'Not Found');
            return true;
          }
          sendJson(res, 200, listingOf(repo, dirTree, path), headers());
          return true;
        }
        const blob = repo.objects.get(entry.oid);
        if (blob === undefined) {
          fail(res, 404, 'Not Found');
          return true;
        }
        sendJson(res, 200, {
          type: 'file',
          encoding: 'base64',
          size: blob.data.length,
          name: entry.name,
          path,
          content: blob.data.toString('base64'),
          sha: entry.oid,
          url: '',
          git_url: '',
          html_url: '',
          download_url: '',
        }, headers());
        return true;
      }
      if (entry.mode !== '40000') {
        fail(res, 404, 'Not Found');
        return true;
      }
      current = repo.objects.get(entry.oid);
    }
    fail(res, 404, 'Not Found');
    return true;
  }

  if (context.method === 'PUT') {
    const parsed = parseJsonBody(context);
    if (!parsed.ok) {
      fail(res, 400, 'Problems parsing JSON');
      return true;
    }
    if (path === '') {
      fail(res, 422, 'Validation Failed: path is required');
      return true;
    }
    const content = str(parsed.value.content);
    const branch = str(parsed.value.branch) === '' ? repo.defaultBranch : str(parsed.value.branch);
    const message = str(parsed.value.message, 'Update file');
    const branchRef = `refs/heads/${branch}`;
    const tip = repo.refs.get(branchRef);
    const files = filesAtCommit(repo, tip === undefined ? undefined : commitOf(repo, tip));
    const before = files.get(path);
    const sha = str(parsed.value.sha);
    if (sha !== '' && (before === undefined || before.oid !== sha)) {
      fail(res, 409, 'sha does not match the current file');
      return true;
    }
    const bytes = Buffer.from(content, 'base64');
    const blobOid = putObject(repo, 'blob', bytes);
    files.set(path, { mode: '100644', oid: blobOid });
    const parent = tip === undefined ? undefined : commitOf(repo, tip);
    const commit = makeCommit(repo, files, parent === undefined ? [] : [parent], message);
    repo.refs.set(branchRef, commit);
    repo.updatedAt = iso(tick());
    sendJson(res, before === undefined ? 201 : 200, {
      content: {
        name: path.split('/').pop() ?? path,
        path,
        sha: blobOid,
        size: bytes.length,
        encoding: 'base64',
        content,
        type: 'file',
      },
      commit: commitJson(commit, message),
    }, headers());
    return true;
  }

  if (context.method === 'DELETE') {
    const parsed = parseJsonBody(context);
    if (!parsed.ok) {
      fail(res, 400, 'Problems parsing JSON');
      return true;
    }
    if (path === '') {
      fail(res, 422, 'Validation Failed: path is required');
      return true;
    }
    const branch = str(parsed.value.branch) === '' ? repo.defaultBranch : str(parsed.value.branch);
    const branchRef = `refs/heads/${branch}`;
    const tip = repo.refs.get(branchRef);
    if (tip === undefined) {
      fail(res, 404, 'Not Found');
      return true;
    }
    const files = filesAtCommit(repo, commitOf(repo, tip));
    const before = files.get(path);
    if (before === undefined) {
      fail(res, 404, 'Not Found');
      return true;
    }
    const sha = str(parsed.value.sha);
    if (sha !== '' && sha !== before.oid) {
      fail(res, 409, 'sha does not match the current file');
      return true;
    }
    files.delete(path);
    const message = str(parsed.value.message, 'Delete file');
    const parent = commitOf(repo, tip);
    const commit = makeCommit(repo, files, parent === undefined ? [] : [parent], message);
    repo.refs.set(branchRef, commit);
    sendJson(res, 200, { content: null, commit: commitJson(commit, message) }, headers());
    return true;
  }

  fail(res, 404, 'Not Found');
  return true;
}

function listingOf(repo: Repo, tree: GitObject, prefix: string): Array<Record<string, unknown>> {
  return parseTree(tree.data).map((entry) => {
    const childPath = prefix === '' ? entry.name : `${prefix}/${entry.name}`;
    if (entry.mode === '40000') {
      return { type: 'dir', name: entry.name, path: childPath, sha: entry.oid, size: 0, mode: '040000', url: '' };
    }
    const blob = repo.objects.get(entry.oid);
    return {
      type: 'file',
      name: entry.name,
      path: childPath,
      sha: entry.oid,
      size: blob === undefined ? 0 : blob.data.length,
      mode: entry.mode === '100755' ? '100755' : '100644',
      url: '',
    };
  });
}

function commitJson(sha: string, message: string): Record<string, unknown> {
  return {
    sha,
    node_id: `C_${sha}`,
    url: '',
    html_url: '',
    author: { name: VIEWER.name, email: VIEWER.email, date: iso(0) },
    committer: { name: VIEWER.name, email: VIEWER.email, date: iso(0) },
    message,
    tree: { sha: '', url: '' },
    parents: [],
  };
}

export type { FlatEntry, Issue };
