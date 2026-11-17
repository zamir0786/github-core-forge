// A small GraphQL v4 executor over the same store: the documents gh and github-mcp-server send
// (viewer, repository reads, __type probes, issue and pull-request reads and mutations, node(id:),
// cursor pagination and operation batching).

import type { ServerResponse } from 'node:http';
import {
  addComment,
  commitOf,
  createIssue,
  createPull,
  diffFiles,
  findRepo,
  mergePull,
  store,
  userOf,
  VIEWER,
  type Comment,
  type Issue,
  type Pull,
  type Repo,
  type User,
} from './store.ts';
import { iso, sendJson, tick } from './util.ts';

// ---------------------------------------------------------------- lexer and parser

type Token = { kind: 'name' | 'int' | 'string' | 'punct' | 'eof'; value: string };

function lex(source: string): Token[] {
  const tokens: Token[] = [];
  let index = 0;
  while (index < source.length) {
    const char = source[index] as string;
    if (char === ' ' || char === '\t' || char === '\n' || char === '\r' || char === ',' || char === '\uFEFF') {
      index += 1;
      continue;
    }
    if (char === '#') {
      while (index < source.length && source[index] !== '\n') index += 1;
      continue;
    }
    if (char === '"') {
      index += 1;
      let text = '';
      while (index < source.length && source[index] !== '"') {
        if (source[index] === '\\') {
          index += 1;
          const escape = source[index] ?? '';
          if (escape === 'n') text += '\n';
          else if (escape === 't') text += '\t';
          else if (escape === 'r') text += '\r';
          else if (escape === 'u') {
            text += String.fromCharCode(Number.parseInt(source.slice(index + 1, index + 5), 16));
            index += 4;
          } else text += escape;
          index += 1;
        } else {
          text += source[index];
          index += 1;
        }
      }
      index += 1;
      tokens.push({ kind: 'string', value: text });
      continue;
    }
    if (char === '-' || (char >= '0' && char <= '9')) {
      let text = char;
      index += 1;
      while (index < source.length && /[0-9.eE+-]/.test(source[index] as string)) {
        text += source[index];
        index += 1;
      }
      tokens.push({ kind: 'int', value: text });
      continue;
    }
    if (/[A-Za-z_]/.test(char)) {
      let text = char;
      index += 1;
      while (index < source.length && /[A-Za-z0-9_]/.test(source[index] as string)) {
        text += source[index];
        index += 1;
      }
      tokens.push({ kind: 'name', value: text });
      continue;
    }
    if (char === '.' && source.startsWith('...', index)) {
      tokens.push({ kind: 'punct', value: '...' });
      index += 3;
      continue;
    }
    tokens.push({ kind: 'punct', value: char });
    index += 1;
  }
  tokens.push({ kind: 'eof', value: '' });
  return tokens;
}

type GqlValue =
  | { kind: 'var'; name: string }
  | { kind: 'int'; value: number }
  | { kind: 'string'; value: string }
  | { kind: 'bool'; value: boolean }
  | { kind: 'null' }
  | { kind: 'enum'; value: string }
  | { kind: 'list'; values: GqlValue[] }
  | { kind: 'object'; fields: Record<string, GqlValue> };

type Selection =
  | { kind: 'field'; alias: string | null; name: string; args: Record<string, GqlValue>; selections: Selection[] }
  | { kind: 'fragment'; typeName: string | null; selections: Selection[] };

interface Operation {
  kind: 'query' | 'mutation';
  selections: Selection[];
}

class Parser {
  private index = 0;
  private readonly tokens: Token[];
  constructor(tokens: Token[]) {
    this.tokens = tokens;
  }

  private peek(): Token {
    return this.tokens[this.index] as Token;
  }

  private next(): Token {
    const token = this.peek();
    this.index += 1;
    return token;
  }

  private expect(value: string): void {
    const token = this.next();
    if (token.value !== value) throw new Error(`expected ${value} but saw ${token.value}`);
  }

  private skipDirectives(): void {
    while (this.peek().value === '@') {
      this.next();
      this.next();
    }
  }

  parseDocument(): Operation[] {
    const operations: Operation[] = [];
    while (this.peek().kind !== 'eof') {
      operations.push(this.parseOperation());
    }
    if (operations.length === 0) throw new Error('empty document');
    return operations;
  }

  private parseOperation(): Operation {
    const token = this.peek();
    if (token.kind === 'punct' && token.value === '{') {
      return { kind: 'query', selections: this.parseSelectionSet() };
    }
    const keyword = this.next().value;
    if (keyword !== 'query' && keyword !== 'mutation' && keyword !== 'subscription') {
      throw new Error(`unsupported operation ${keyword}`);
    }
    if (this.peek().kind === 'name') this.next();
    if (this.peek().value === '(') {
      this.next();
      let depth = 1;
      while (depth > 0 && this.peek().kind !== 'eof') {
        const current = this.next();
        if (current.value === '(') depth += 1;
        else if (current.value === ')') depth -= 1;
      }
    }
    this.skipDirectives();
    return { kind: keyword === 'mutation' ? 'mutation' : 'query', selections: this.parseSelectionSet() };
  }

  private parseSelectionSet(): Selection[] {
    this.expect('{');
    const selections: Selection[] = [];
    while (this.peek().value !== '}' && this.peek().kind !== 'eof') {
      selections.push(this.parseSelection());
    }
    this.expect('}');
    return selections;
  }

  private parseSelection(): Selection {
    if (this.peek().value === '...') {
      this.next();
      let typeName: string | null = null;
      if (this.peek().value === 'on') {
        this.next();
        typeName = this.next().value;
      } else if (this.peek().kind === 'name') {
        this.next();
      }
      this.skipDirectives();
      if (this.peek().value === '{') return { kind: 'fragment', typeName, selections: this.parseSelectionSet() };
      return { kind: 'fragment', typeName, selections: [] };
    }
    let name = this.next().value;
    let alias: string | null = null;
    if (this.peek().value === ':') {
      this.next();
      alias = name;
      name = this.next().value;
    }
    let args: Record<string, GqlValue> = {};
    if (this.peek().value === '(') args = this.parseArgs();
    this.skipDirectives();
    const selections = this.peek().value === '{' ? this.parseSelectionSet() : [];
    return { kind: 'field', alias, name, args, selections };
  }

  private parseArgs(): Record<string, GqlValue> {
    this.expect('(');
    const args: Record<string, GqlValue> = {};
    while (this.peek().value !== ')' && this.peek().kind !== 'eof') {
      const name = this.next().value;
      this.expect(':');
      args[name] = this.parseValue();
    }
    this.expect(')');
    return args;
  }

  private parseValue(): GqlValue {
    const token = this.next();
    if (token.value === '$') return { kind: 'var', name: this.next().value };
    if (token.value === '[') {
      const values: GqlValue[] = [];
      while (this.peek().value !== ']' && this.peek().kind !== 'eof') values.push(this.parseValue());
      this.expect(']');
      return { kind: 'list', values };
    }
    if (token.value === '{') {
      const fields: Record<string, GqlValue> = {};
      while (this.peek().value !== '}' && this.peek().kind !== 'eof') {
        const name = this.next().value;
        this.expect(':');
        fields[name] = this.parseValue();
      }
      this.expect('}');
      return { kind: 'object', fields };
    }
    if (token.kind === 'string') return { kind: 'string', value: token.value };
    if (token.kind === 'int') return { kind: 'int', value: Number(token.value) };
    if (token.value === 'true') return { kind: 'bool', value: true };
    if (token.value === 'false') return { kind: 'bool', value: false };
    if (token.value === 'null') return { kind: 'null' };
    return { kind: 'enum', value: token.value };
  }
}

function evalValue(value: GqlValue | undefined, vars: Record<string, unknown>): unknown {
  if (value === undefined) return undefined;
  switch (value.kind) {
    case 'var':
      return vars[value.name];
    case 'int':
      return value.value;
    case 'string':
      return value.value;
    case 'bool':
      return value.value;
    case 'null':
      return null;
    case 'enum':
      return value.value;
    case 'list':
      return value.values.map((entry) => evalValue(entry, vars));
    case 'object': {
      const out: Record<string, unknown> = {};
      for (const [key, entry] of Object.entries(value.fields)) out[key] = evalValue(entry, vars);
      return out;
    }
    default:
      return undefined;
  }
}

function evalArgs(args: Record<string, GqlValue>, vars: Record<string, unknown>): Record<string, unknown> {
  const out: Record<string, unknown> = {};
  for (const [key, value] of Object.entries(args)) out[key] = evalValue(value, vars);
  return out;
}

// ---------------------------------------------------------------- global ids and wrappers

function globalId(type: string, id: number): string {
  return Buffer.from(`${type}:${id}`, 'utf8').toString('base64');
}

function decodeId(text: unknown): { type: string; id: number } | null {
  if (typeof text !== 'string') return null;
  let decoded: string;
  try {
    decoded = Buffer.from(text, 'base64').toString('utf8');
  } catch {
    return null;
  }
  const colon = decoded.indexOf(':');
  if (colon < 0) return null;
  const type = decoded.slice(0, colon);
  const id = Number.parseInt(decoded.slice(colon + 1), 10);
  if (!Number.isFinite(id)) return null;
  return { type, id };
}

interface Wrapped {
  type: string;
  ref: unknown;
}

function wrap(type: string, ref: unknown): Wrapped {
  return { type, ref };
}

function isWrapped(value: unknown): value is Wrapped {
  return typeof value === 'object' && value !== null && 'type' in value && 'ref' in value;
}

interface IssueRef {
  repo: Repo;
  issue: Issue;
}

interface PullRef {
  repo: Repo;
  pull: Pull;
}

function findIssueById(id: number): IssueRef | null {
  for (const repo of store.repos.values()) {
    const issue = repo.issues.find((entry) => entry.id === id);
    if (issue !== undefined) return { repo, issue };
  }
  return null;
}

function findPullById(id: number): PullRef | null {
  for (const repo of store.repos.values()) {
    const pull = repo.pulls.find((entry) => entry.id === id);
    if (pull !== undefined) return { repo, pull };
  }
  return null;
}

function findRepoById(id: number): Repo | null {
  for (const repo of store.repos.values()) if (repo.id === id) return repo;
  return null;
}

// ---------------------------------------------------------------- schema description

const SCHEMA_FIELDS: Record<string, readonly string[]> = {
  Issue: ['id', 'number', 'title', 'state', 'body', 'comments', 'labels', 'author', 'createdAt', 'updatedAt', 'closed', 'repository'],
  PullRequest: [
    'id',
    'number',
    'title',
    'body',
    'state',
    'merged',
    'mergeable',
    'headRefName',
    'baseRefName',
    'files',
    'comments',
    'author',
    'createdAt',
    'updatedAt',
  ],
  Repository: [
    'id',
    'databaseId',
    'name',
    'nameWithOwner',
    'owner',
    'description',
    'defaultBranchRef',
    'viewerPermission',
    'hasIssuesEnabled',
    'isPrivate',
    'issue',
    'issues',
    'pullRequests',
    'pullRequest',
  ],
};

const SCHEMA_ENUMS: Record<string, readonly string[]> = {
  SearchType: ['ISSUE', 'PULL_REQUEST', 'REPOSITORY', 'USER'],
  IssueState: ['OPEN', 'CLOSED'],
  PullRequestState: ['OPEN', 'CLOSED', 'MERGED'],
};

// ---------------------------------------------------------------- execution

interface ExecContext {
  vars: Record<string, unknown>;
  errors: Array<{ message: string; locations?: unknown[]; path?: unknown[] }>;
}

function connPage(values: readonly unknown[], args: Record<string, unknown>): { items: unknown[]; hasNext: boolean; endCursor: string } {
  const firstRaw = typeof args.first === 'number' ? args.first : 100;
  const first = Math.max(1, Math.min(100, firstRaw));
  let offset = 0;
  if (typeof args.after === 'string' && args.after !== '') {
    try {
      const decoded = Buffer.from(args.after, 'base64').toString('utf8');
      const parsed = Number.parseInt(decoded.slice(decoded.indexOf(':') + 1), 10);
      if (Number.isFinite(parsed) && parsed >= 0) offset = parsed;
    } catch {
      offset = 0;
    }
  }
  const items = values.slice(offset, offset + first);
  const end = offset + items.length;
  return {
    items,
    hasNext: end < values.length,
    endCursor: Buffer.from(`cursor:${end}`, 'utf8').toString('base64'),
  };
}

function issueConnection(conn: { repo: Repo; args: Record<string, unknown> }): Wrapped {
  let list = [...conn.repo.issues];
  const states = conn.args.states;
  if (Array.isArray(states) && states.length > 0) {
    const wanted = states.map((state) => String(state).toUpperCase());
    list = list.filter((issue) => wanted.includes(issue.state.toUpperCase()));
  }
  const orderBy = conn.args.orderBy;
  let direction = 'ASC';
  if (typeof orderBy === 'object' && orderBy !== null) {
    const field = (orderBy as Record<string, unknown>).direction;
    if (typeof field === 'string') direction = field.toUpperCase();
  }
  list.sort((a, b) => (direction === 'DESC' ? b.number - a.number : a.number - b.number));
  const page = connPage(list, conn.args);
  return wrap('IssueConnection', {
    nodes: page.items.map((issue) => wrap('Issue', { repo: conn.repo, issue } as IssueRef)),
    hasNext: page.hasNext,
    endCursor: page.endCursor,
  });
}

function pullConnection(conn: { repo: Repo; args: Record<string, unknown> }): Wrapped {
  const list = [...conn.repo.pulls].sort((a, b) => a.number - b.number);
  const page = connPage(list, conn.args);
  return wrap('PullRequestConnection', {
    nodes: page.items.map((pull) => wrap('PullRequest', { repo: conn.repo, pull } as PullRef)),
    hasNext: page.hasNext,
    endCursor: page.endCursor,
  });
}

function findIssue(repo: Repo, number: unknown): Issue | undefined {
  const parsed = typeof number === 'number' ? number : Number.parseInt(String(number ?? ''), 10);
  return repo.issues.find((issue) => issue.number === parsed);
}

function findPull(repo: Repo, number: unknown): Pull | undefined {
  const parsed = typeof number === 'number' ? number : Number.parseInt(String(number ?? ''), 10);
  return repo.pulls.find((pull) => pull.number === parsed);
}

function issueState(issue: Issue): string {
  return issue.state.toUpperCase();
}

function pullState(pull: Pull): string {
  return pull.merged ? 'MERGED' : pull.state.toUpperCase();
}

function resolve(type: string, ref: unknown, field: string, args: Record<string, unknown>, ctx: ExecContext): unknown {
  switch (type) {
    case 'Query':
      if (field === 'viewer') return wrap('Viewer', VIEWER);
      if (field === 'repository') {
        const repo = findRepo(String(args.owner ?? ''), String(args.name ?? ''));
        return repo === undefined ? null : wrap('Repository', repo);
      }
      if (field === 'node') {
        const decoded = decodeId(args.id);
        if (decoded === null) return null;
        if (decoded.type === 'Repository') {
          const repo = findRepoById(decoded.id);
          return repo === null ? null : wrap('Repository', repo);
        }
        if (decoded.type === 'Issue') {
          const found = findIssueById(decoded.id);
          return found === null ? null : wrap('Issue', found);
        }
        if (decoded.type === 'PullRequest') {
          const found = findPullById(decoded.id);
          return found === null ? null : wrap('PullRequest', found);
        }
        return null;
      }
      if (field === '__type') {
        const name = String(args.name ?? '');
        if (!(name in SCHEMA_FIELDS) && !(name in SCHEMA_ENUMS)) return null;
        return wrap('__Type', name);
      }
      if (field === '__schema') return wrap('__Schema', null);
      break;
    case 'Mutation':
      return resolveMutation(field, args);
    case 'Viewer': {
      const user = ref as User;
      if (field === 'login') return user.login;
      if (field === 'name') return user.name;
      if (field === 'id') return globalId('User', user.id);
      if (field === 'databaseId') return user.id;
      break;
    }
    case 'User': {
      const user = ref as User;
      if (field === 'login') return user.login;
      if (field === 'name') return user.name;
      if (field === 'id') return globalId('User', user.id);
      if (field === 'databaseId') return user.id;
      break;
    }
    case 'Repository': {
      const repo = ref as Repo;
      if (field === 'id') return globalId('Repository', repo.id);
      if (field === 'databaseId') return repo.id;
      if (field === 'name') return repo.name;
      if (field === 'nameWithOwner') return `${repo.owner}/${repo.name}`;
      if (field === 'description') return repo.description;
      if (field === 'owner') return wrap('User', userOf(repo.owner));
      if (field === 'defaultBranchRef') {
        const has = repo.refs.has(`refs/heads/${repo.defaultBranch}`);
        return has ? wrap('Ref', { name: repo.defaultBranch }) : null;
      }
      if (field === 'viewerPermission') return 'ADMIN';
      if (field === 'hasIssuesEnabled') return true;
      if (field === 'isPrivate') return repo.private;
      if (field === 'issue') {
        const issue = findIssue(repo, args.number);
        return issue === undefined ? null : wrap('Issue', { repo, issue } as IssueRef);
      }
      if (field === 'issues') return issueConnection({ repo, args });
      if (field === 'pullRequests') return pullConnection({ repo, args });
      if (field === 'pullRequest') {
        const pull = findPull(repo, args.number);
        return pull === undefined ? null : wrap('PullRequest', { repo, pull } as PullRef);
      }
      break;
    }
    case 'Ref': {
      const value = ref as { name: string };
      if (field === 'name') return value.name;
      if (field === 'target') return null;
      break;
    }
    case 'Issue': {
      const { issue } = ref as IssueRef;
      if (field === 'id') return globalId('Issue', issue.id);
      if (field === 'number') return issue.number;
      if (field === 'title') return issue.title;
      if (field === 'body') return issue.body;
      if (field === 'state') return issueState(issue);
      if (field === 'comments') return wrap('IssueCommentConnection', { comments: [] });
      if (field === 'labels') return wrap('LabelConnection', { labels: issue.labels });
      if (field === 'author') return wrap('User', VIEWER);
      if (field === 'createdAt') return issue.created_at;
      if (field === 'updatedAt') return issue.updated_at;
      if (field === 'closed') return issue.state === 'closed';
      break;
    }
    case 'IssueConnection': {
      const conn = ref as { nodes: unknown[]; hasNext: boolean; endCursor: string };
      if (field === 'nodes') return conn.nodes;
      if (field === 'totalCount') return conn.nodes.length;
      if (field === 'pageInfo') return wrap('PageInfo', conn);
      break;
    }
    case 'PageInfo': {
      const conn = ref as { hasNext: boolean; endCursor: string };
      if (field === 'hasNextPage') return conn.hasNext;
      if (field === 'hasPreviousPage') return false;
      if (field === 'endCursor') return conn.endCursor;
      if (field === 'startCursor') return conn.endCursor;
      break;
    }
    case 'PullRequestConnection': {
      const conn = ref as { nodes: unknown[]; hasNext: boolean; endCursor: string };
      if (field === 'nodes') return conn.nodes;
      if (field === 'totalCount') return conn.nodes.length;
      if (field === 'pageInfo') return wrap('PageInfo', conn);
      break;
    }
    case 'PullRequest': {
      const { repo, pull } = ref as PullRef;
      if (field === 'id') return globalId('PullRequest', pull.id);
      if (field === 'number') return pull.number;
      if (field === 'title') return pull.title;
      if (field === 'body') return pull.body;
      if (field === 'state') return pullState(pull);
      if (field === 'merged') return pull.merged;
      if (field === 'mergeable') return 'MERGEABLE';
      if (field === 'headRefName') return pull.headRef;
      if (field === 'baseRefName') return pull.baseRef;
      if (field === 'files') {
        const baseCommit = commitOf(repo, pull.baseSha);
        const headCommit = commitOf(repo, pull.headSha);
        const files = diffFiles(repo, baseCommit, headCommit);
        const page = connPage(files, args);
        return wrap('PullRequestFileConnection', {
          nodes: page.items.map((file) => wrap('PullRequestFile', file)),
          hasNext: page.hasNext,
          endCursor: page.endCursor,
        });
      }
      if (field === 'author') return wrap('User', VIEWER);
      if (field === 'createdAt') return pull.created_at;
      if (field === 'updatedAt') return pull.updated_at;
      break;
    }
    case 'PullRequestFileConnection': {
      const conn = ref as { nodes: unknown[]; hasNext: boolean; endCursor: string };
      if (field === 'nodes') return conn.nodes;
      if (field === 'totalCount') return conn.nodes.length;
      if (field === 'pageInfo') return wrap('PageInfo', conn);
      break;
    }
    case 'PullRequestFile': {
      const file = ref as { path?: string; filename?: string };
      if (field === 'path') return file.path ?? file.filename ?? '';
      if (field === 'additions') return 1;
      if (field === 'deletions') return 0;
      break;
    }
    case '__Type': {
      const name = ref as string;
      if (field === 'name') return name;
      if (field === 'kind') return name in SCHEMA_ENUMS ? 'ENUM' : 'OBJECT';
      if (field === 'fields') {
        const names = SCHEMA_FIELDS[name] ?? [];
        return names.map((value) => wrap('__Field', value));
      }
      if (field === 'enumValues') {
        const names = SCHEMA_ENUMS[name] ?? [];
        return names.map((value) => wrap('__EnumValue', value));
      }
      break;
    }
    case '__Field': {
      const name = ref as string;
      if (field === 'name') return name;
      if (field === 'isDeprecated') return false;
      if (field === 'deprecationReason') return null;
      break;
    }
    case '__EnumValue': {
      const name = ref as string;
      if (field === 'name') return name;
      if (field === 'isDeprecated') return false;
      if (field === 'deprecationReason') return null;
      break;
    }
    case 'CreateIssuePayload': {
      const payload = ref as { issue: IssueRef };
      if (field === 'issue') return wrap('Issue', payload.issue);
      break;
    }
    case 'CloseIssuePayload': {
      const payload = ref as { issue: IssueRef };
      if (field === 'issue') return wrap('Issue', payload.issue);
      break;
    }
    case 'CreatePullRequestPayload': {
      const payload = ref as { pull: PullRef };
      if (field === 'pullRequest') return wrap('PullRequest', payload.pull);
      break;
    }
    case 'MergePullRequestPayload': {
      const payload = ref as { pull: PullRef };
      if (field === 'pullRequest') return wrap('PullRequest', payload.pull);
      break;
    }
    case 'AddCommentPayload': {
      const payload = ref as { comment: Comment };
      if (field === 'commentEdge') return wrap('CommentEdge', payload.comment);
      if (field === 'comment') return wrap('Comment', payload.comment);
      break;
    }
    case 'CommentEdge': {
      const comment = ref as Comment;
      if (field === 'node') return wrap('Comment', comment);
      break;
    }
    case 'Comment': {
      const comment = ref as Comment;
      if (field === 'id') return globalId('Comment', comment.id);
      if (field === 'body') return comment.body;
      if (field === 'author') return wrap('User', comment.user);
      if (field === 'createdAt') return comment.created_at;
      break;
    }
    case 'IssueCommentConnection':
    case 'LabelConnection':
      break;
    default:
      break;
  }
  ctx.errors.push({ message: `Field '${field}' doesn't exist on type '${type}'` });
  return null;
}

function resolveMutation(field: string, args: Record<string, unknown>): unknown {
  const input = (args.input ?? {}) as Record<string, unknown>;
  if (field === 'createIssue') {
    const decoded = decodeId(input.repositoryId);
    const repo = decoded === null ? null : findRepoById(decoded.id);
    if (repo === null) return null;
    const issue = createIssue(repo, String(input.title ?? ''), String(input.body ?? ''));
    return wrap('CreateIssuePayload', { issue: { repo, issue } as IssueRef });
  }
  if (field === 'closeIssue') {
    const decoded = decodeId(input.issueId);
    const found = decoded === null ? null : findIssueById(decoded.id);
    if (found === null) return null;
    found.issue.state = 'closed';
    found.issue.updated_at = iso(tick());
    found.issue.cache = undefined;
    return wrap('CloseIssuePayload', { issue: found });
  }
  if (field === 'createPullRequest') {
    const decoded = decodeId(input.repositoryId);
    const repo = decoded === null ? null : findRepoById(decoded.id);
    if (repo === null) return null;
    const created = createPull(repo, {
      title: String(input.title ?? ''),
      body: String(input.body ?? ''),
      head: String(input.headRefName ?? ''),
      base: String(input.baseRefName ?? ''),
    });
    if (typeof created === 'string') return null;
    return wrap('CreatePullRequestPayload', { pull: { repo, pull: created } as PullRef });
  }
  if (field === 'mergePullRequest') {
    const decoded = decodeId(input.pullRequestId);
    const found = decoded === null ? null : findPullById(decoded.id);
    if (found === null) return null;
    const result = mergePull(found.repo, found.pull);
    if (!result.ok) return null;
    return wrap('MergePullRequestPayload', { pull: found });
  }
  if (field === 'addComment') {
    const decoded = decodeId(input.subjectId);
    const found = decoded === null ? null : findIssueById(decoded.id);
    if (found === null) return null;
    const comment = addComment(found.repo, found.issue, String(input.body ?? ''));
    return wrap('AddCommentPayload', { comment });
  }
  return null;
}

function executeSelections(
  parent: Wrapped,
  selections: readonly Selection[],
  ctx: ExecContext,
  out: Record<string, unknown>,
): void {
  for (const selection of selections) {
    if (selection.kind === 'fragment') {
      if (selection.typeName !== null && selection.typeName !== parent.type) continue;
      executeSelections(parent, selection.selections, ctx, out);
      continue;
    }
    if (selection.name === '__typename') {
      out[selection.alias ?? selection.name] = parent.type;
      continue;
    }
    const args = evalArgs(selection.args, ctx.vars);
    const value = resolve(parent.type, parent.ref, selection.name, args, ctx);
    const key = selection.alias ?? selection.name;
    if (selection.selections.length === 0) {
      out[key] = value === undefined ? null : value;
      continue;
    }
    if (value === null || value === undefined) {
      out[key] = null;
      continue;
    }
    if (Array.isArray(value)) {
      out[key] = value.map((item) => {
        if (!isWrapped(item)) return item;
        const child: Record<string, unknown> = {};
        executeSelections(item, selection.selections, ctx, child);
        return child;
      });
      continue;
    }
    if (isWrapped(value)) {
      const child: Record<string, unknown> = {};
      executeSelections(value, selection.selections, ctx, child);
      out[key] = child;
      continue;
    }
    out[key] = value;
  }
}

function executeOperation(operation: Operation, vars: Record<string, unknown>): Record<string, unknown> {
  const ctx: ExecContext = { vars, errors: [] };
  const rootType = operation.kind === 'mutation' ? 'Mutation' : 'Query';
  const data: Record<string, unknown> = {};
  executeSelections(wrap(rootType, null), operation.selections, ctx, data);
  if (ctx.errors.length > 0) (data as Record<string, unknown>).__errors = ctx.errors;
  return data;
}

/** Run one GraphQL document; answers `{data, errors?}` exactly as GitHub v4 does. */
export function runGraphql(query: string, variables: Record<string, unknown>): Record<string, unknown> {
  let operations: Operation[];
  try {
    operations = new Parser(lex(query)).parseDocument();
  } catch (err) {
    return { errors: [{ message: err instanceof Error ? err.message : String(err) }] };
  }
  const operation = operations[0];
  if (operation === undefined) return { errors: [{ message: 'no operation' }] };
  const ctxVars = variables;
  const data = executeOperation(operation, ctxVars);
  const errors = data.__errors as Array<{ message: string }> | undefined;
  delete data.__errors;
  if (errors !== undefined && errors.length > 0) return { data, errors };
  return { data };
}

/** POST /api/graphql: one document, or a batch array of them. */
export function handleGraphql(res: ServerResponse, bodyText: string): void {
  let parsed: unknown;
  try {
    parsed = JSON.parse(bodyText);
  } catch {
    sendJson(res, 200, { errors: [{ message: 'Problems parsing JSON' }] });
    return;
  }
  if (Array.isArray(parsed)) {
    const answers = parsed.map((entry) => {
      const document = (entry ?? {}) as Record<string, unknown>;
      const query = typeof document.query === 'string' ? document.query : '';
      const variables = typeof document.variables === 'object' && document.variables !== null ? (document.variables as Record<string, unknown>) : {};
      return runGraphql(query, variables);
    });
    sendJson(res, 200, answers);
    return;
  }
  if (typeof parsed !== 'object' || parsed === null) {
    sendJson(res, 400, { errors: [{ message: 'Problems parsing JSON' }] });
    return;
  }
  const document = parsed as Record<string, unknown>;
  const query = typeof document.query === 'string' ? document.query : '';
  const variables = typeof document.variables === 'object' && document.variables !== null ? (document.variables as Record<string, unknown>) : {};
  sendJson(res, 200, runGraphql(query, variables));
}
