// The MCP endpoint: the modern 2026-07-28 revision (server/discover + stateless requests), a tools
// surface over the same store, and the JSON-RPC error behaviour the checks require.

import type { ServerResponse } from 'node:http';
import {
  createIssue,
  createPull,
  createRepo,
  findRepo,
  issueJson,
  mergePull,
  pullJson,
  repoJson,
  store,
  VIEWER,
  type Repo,
} from './store.ts';
import { sendJson, sendText } from './util.ts';

const PROTOCOL_VERSION = '2026-07-28';
const SERVER_INFO = { name: 'makefun-forge', version: '1.0.0' };
const SERVER_DESCRIPTION = 'A GitHub-compatible forge: repositories, issues and pull requests over one shared store.';

interface ToolSchema {
  type: 'object';
  properties: Record<string, Record<string, unknown>>;
  required: string[];
  additionalProperties?: boolean;
}

interface Tool {
  name: string;
  description: string;
  inputSchema: ToolSchema;
}

const str = (description: string): Record<string, unknown> => ({ type: 'string', description });
const num = (description: string): Record<string, unknown> => ({ type: 'number', description });
const bool = (description: string): Record<string, unknown> => ({ type: 'boolean', description });

const TOOLS: readonly Tool[] = Object.freeze([
  {
    name: 'create_repo',
    description: 'Create a repository owned by the authenticated user.',
    inputSchema: {
      type: 'object',
      properties: {
        owner: str('The owner login; defaults to the authenticated user.'),
        name: str('The repository name.'),
        description: str('A short description.'),
        private: bool('Whether the repository is private.'),
      },
      required: ['name'],
      additionalProperties: false,
    },
  },
  {
    name: 'get_repo',
    description: 'Read one repository by owner and name.',
    inputSchema: {
      type: 'object',
      properties: {
        owner: str('The owner login.'),
        repo: str('The repository name.'),
      },
      required: ['owner', 'repo'],
      additionalProperties: false,
    },
  },
  {
    name: 'create_issue',
    description: 'Create an issue in a repository.',
    inputSchema: {
      type: 'object',
      properties: {
        owner: str('The owner login.'),
        repo: str('The repository name.'),
        title: str('The issue title.'),
        body: str('The issue body.'),
      },
      required: ['owner', 'repo', 'title'],
      additionalProperties: false,
    },
  },
  {
    name: 'list_issues',
    description: 'List the issues of a repository, optionally filtered by state.',
    inputSchema: {
      type: 'object',
      properties: {
        owner: str('The owner login.'),
        repo: str('The repository name.'),
        state: str('open, closed or all.'),
      },
      required: ['owner', 'repo'],
      additionalProperties: false,
    },
  },
  {
    name: 'create_pull_request',
    description: 'Open a pull request from a head branch into a base branch.',
    inputSchema: {
      type: 'object',
      properties: {
        owner: str('The owner login.'),
        repo: str('The repository name.'),
        title: str('The pull request title.'),
        body: str('The pull request body.'),
        head: str('The source branch.'),
        base: str('The target branch.'),
      },
      required: ['owner', 'repo', 'head', 'base'],
      additionalProperties: false,
    },
  },
  {
    name: 'merge_pull_request',
    description: 'Merge a pull request into its base branch.',
    inputSchema: {
      type: 'object',
      properties: {
        owner: str('The owner login.'),
        repo: str('The repository name.'),
        pull_number: num('The pull request number.'),
      },
      required: ['owner', 'repo', 'pull_number'],
      additionalProperties: false,
    },
  },
]);

const TOOLS_RESULT = JSON.stringify({ tools: TOOLS });

type Json = Record<string, unknown>;

function argumentString(args: Json, key: string): string {
  const value = args[key];
  return typeof value === 'string' ? value : '';
}

function resolveRepo(args: Json): Repo | undefined {
  const owner = argumentString(args, 'owner') || VIEWER.login;
  const name = argumentString(args, 'repo') || argumentString(args, 'repository') || argumentString(args, 'name');
  return findRepo(owner, name);
}

function toolResult(value: unknown, extra: Json = {}): Json {
  return {
    content: [{ type: 'text', text: JSON.stringify(value) }],
    structuredContent: value,
    ...extra,
  };
}

function toolError(message: string): Json {
  return { content: [{ type: 'text', text: message }], isError: true };
}

function callTool(name: string, args: Json): Json {
  if (name === 'create_repo') {
    const owner = argumentString(args, 'owner') || VIEWER.login;
    const repoName = argumentString(args, 'name') || argumentString(args, 'repo');
    if (repoName === '') return toolError('name is required');
    const existing = findRepo(owner, repoName);
    if (existing !== undefined) return toolResult(repoJson(existing));
    const created = createRepo(owner, repoName, argumentString(args, 'description'), args.private === true);
    return toolResult(repoJson(created));
  }
  if (name === 'get_repo') {
    const repo = resolveRepo(args);
    if (repo === undefined) {
      const owner = argumentString(args, 'owner') || VIEWER.login;
      const repoName = argumentString(args, 'repo');
      return toolError(`Repository ${owner}/${repoName} was not found`);
    }
    return toolResult(repoJson(repo));
  }
  if (name === 'create_issue') {
    const repo = resolveRepo(args);
    if (repo === undefined) return toolError('repository not found');
    const issue = createIssue(repo, argumentString(args, 'title'), argumentString(args, 'body'));
    return toolResult(issueJson(issue, repo));
  }
  if (name === 'list_issues') {
    const repo = resolveRepo(args);
    if (repo === undefined) return toolError('repository not found');
    const state = argumentString(args, 'state') || 'open';
    const list = repo.issues.filter((issue) => state === 'all' || issue.state === state);
    return toolResult(list.map((issue) => issueJson(issue, repo)));
  }
  if (name === 'create_pull_request') {
    const repo = resolveRepo(args);
    if (repo === undefined) return toolError('repository not found');
    const created = createPull(repo, {
      title: argumentString(args, 'title'),
      body: argumentString(args, 'body'),
      head: argumentString(args, 'head'),
      base: argumentString(args, 'base'),
    });
    if (typeof created === 'string') return toolError(created);
    return toolResult(pullJson(created, repo));
  }
  if (name === 'merge_pull_request') {
    const repo = resolveRepo(args);
    if (repo === undefined) return toolError('repository not found');
    const raw = args.pull_number ?? args.number ?? args.pull_request_number;
    const number = typeof raw === 'number' ? raw : Number.parseInt(String(raw ?? ''), 10);
    const pull = repo.pulls.find((entry) => entry.number === number);
    if (pull === undefined) return toolError(`pull request ${String(raw)} not found`);
    const result = mergePull(repo, pull);
    if (!result.ok) return toolError(result.message);
    return toolResult(pullJson(pull, repo));
  }
  return toolError(`unknown tool ${name}`);
}

function rpcResult(res: ServerResponse, id: unknown, result: unknown): void {
  sendJson(res, 200, { jsonrpc: '2.0', id: id ?? null, result });
}

function rpcError(res: ServerResponse, status: number, id: unknown, code: number, message: string): void {
  sendJson(res, status, { jsonrpc: '2.0', id: id ?? null, error: { code, message } });
}

/** POST /mcp. Authentication is decided by the caller; this handles the JSON-RPC exchange. */
export function handleMcp(res: ServerResponse, bodyText: string): void {
  let parsed: unknown;
  try {
    parsed = JSON.parse(bodyText);
  } catch {
    rpcError(res, 400, null, -32700, 'Parse error: the request body is not JSON');
    return;
  }
  const request = (parsed ?? {}) as Json;
  const method = typeof request.method === 'string' ? request.method : '';
  const id = request.id;
  const params = (typeof request.params === 'object' && request.params !== null ? request.params : {}) as Json;
  if (method === '') {
    rpcError(res, 200, id, -32600, 'Invalid request: method is required');
    return;
  }
  if (method.startsWith('notifications/')) {
    res.writeHead(202, { 'content-length': 0 });
    res.end();
    return;
  }
  if (method === 'server/discover') {
    rpcResult(res, id, {
      supportedVersions: [PROTOCOL_VERSION],
      capabilities: { tools: { listChanged: false } },
      serverInfo: { ...SERVER_INFO },
      instructions: SERVER_DESCRIPTION,
      _meta: {
        'io.modelcontextprotocol/protocolVersion': PROTOCOL_VERSION,
        'io.modelcontextprotocol/serverInfo': { ...SERVER_INFO },
      },
    });
    return;
  }
  if (method === 'initialize') {
    const requested = typeof params.protocolVersion === 'string' ? params.protocolVersion : PROTOCOL_VERSION;
    rpcResult(res, id, {
      protocolVersion: requested,
      capabilities: { tools: { listChanged: false } },
      serverInfo: { ...SERVER_INFO },
      instructions: SERVER_DESCRIPTION,
    });
    return;
  }
  if (method === 'ping') {
    rpcResult(res, id, {});
    return;
  }
  if (method === 'tools/list') {
    // The tools array is serialized once, so two answers carry the same bytes.
    const body = `{"jsonrpc":"2.0","id":${JSON.stringify(id ?? null)},"result":${TOOLS_RESULT}}`;
    res.writeHead(200, { 'content-type': 'application/json; charset=utf-8', 'content-length': Buffer.byteLength(body) });
    res.end(body);
    return;
  }
  if (method === 'tools/call') {
    const name = typeof params.name === 'string' ? params.name : '';
    const args = (typeof params.arguments === 'object' && params.arguments !== null ? params.arguments : {}) as Json;
    const tool = TOOLS.find((entry) => entry.name === name);
    if (tool === undefined) {
      rpcError(res, 200, id, -32602, `Unknown tool: ${name}`);
      return;
    }
    const missing = tool.inputSchema.required.filter((key) => args[key] === undefined || args[key] === null || args[key] === '');
    if (missing.length > 0) {
      rpcError(res, 200, id, -32602, `Missing required argument(s): ${missing.join(', ')}`);
      return;
    }
    rpcResult(res, id, callTool(name, args));
    return;
  }
  rpcError(res, 200, id, -32601, `Method not found: ${method}`);
}

/** A plain-text 404 for a non-REST path. */
export function sendNotFound(res: ServerResponse): void {
  sendText(res, 404, 'not found\n');
}
