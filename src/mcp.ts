// MCP over Streamable HTTP (POST /mcp): JSON-RPC 2.0, both eras — modern
// `server/discover` (2026-07-28) and the legacy `initialize` handshake. Tools act
// on the same store as REST, so effects are visible over REST.

import type { IncomingMessage, ServerResponse } from 'node:http';
import { isRecord, readBody, type Ctx } from './http.ts';
import { VIEWER } from './scenario.ts';
import { gid, type Forge, type Issue, type Pull, type Repo } from './store.ts';

const SERVER_NAME = 'github-core-clone-forge';
const SERVER_VERSION = '1.0.0';
const SUPPORTED_VERSIONS = ['2026-07-28', '2025-11-25', '2025-06-18', '2025-03-26', '2024-11-05'];

interface ToolDef {
  name: string;
  description: string;
  inputSchema: Record<string, unknown>;
  required: string[];
}

const TOOLS: ToolDef[] = [
  {
    name: 'create_repo',
    description: 'Create a repository owned by the authenticated user.',
    inputSchema: {
      type: 'object',
      properties: {
        name: { type: 'string', description: 'Repository name' },
        description: { type: 'string', description: 'Repository description' },
        private: { type: 'boolean', description: 'Whether the repository is private' },
      },
      required: ['name'],
    },
    required: ['name'],
  },
  {
    name: 'get_repo',
    description: 'Get a repository by owner and name.',
    inputSchema: {
      type: 'object',
      properties: {
        owner: { type: 'string', description: 'Repository owner login' },
        repo: { type: 'string', description: 'Repository name' },
      },
      required: ['owner', 'repo'],
    },
    required: ['owner', 'repo'],
  },
  {
    name: 'create_issue',
    description: 'Create an issue in a repository.',
    inputSchema: {
      type: 'object',
      properties: {
        owner: { type: 'string', description: 'Repository owner login' },
        repo: { type: 'string', description: 'Repository name' },
        title: { type: 'string', description: 'Issue title' },
        body: { type: 'string', description: 'Issue body' },
      },
      required: ['owner', 'repo', 'title'],
    },
    required: ['owner', 'repo', 'title'],
  },
  {
    name: 'list_issues',
    description: 'List issues of a repository.',
    inputSchema: {
      type: 'object',
      properties: {
        owner: { type: 'string', description: 'Repository owner login' },
        repo: { type: 'string', description: 'Repository name' },
        state: { type: 'string', description: 'Issue state: open, closed or all' },
      },
      required: ['owner', 'repo'],
    },
    required: ['owner', 'repo'],
  },
  {
    name: 'create_pull_request',
    description: 'Open a pull request from a head branch into a base branch.',
    inputSchema: {
      type: 'object',
      properties: {
        owner: { type: 'string', description: 'Repository owner login' },
        repo: { type: 'string', description: 'Repository name' },
        title: { type: 'string', description: 'Pull request title' },
        body: { type: 'string', description: 'Pull request body' },
        head: { type: 'string', description: 'Head branch name' },
        base: { type: 'string', description: 'Base branch name' },
      },
      required: ['owner', 'repo', 'title', 'head', 'base'],
    },
    required: ['owner', 'repo', 'title', 'head', 'base'],
  },
  {
    name: 'merge_pull_request',
    description: 'Merge a pull request with a squash merge.',
    inputSchema: {
      type: 'object',
      properties: {
        owner: { type: 'string', description: 'Repository owner login' },
        repo: { type: 'string', description: 'Repository name' },
        pull_number: { type: 'integer', description: 'Pull request number' },
      },
      required: ['owner', 'repo', 'pull_number'],
    },
    required: ['owner', 'repo', 'pull_number'],
  },
  {
    name: 'get_issue',
    description: 'Get one issue of a repository by number.',
    inputSchema: {
      type: 'object',
      properties: {
        owner: { type: 'string', description: 'Repository owner login' },
        repo: { type: 'string', description: 'Repository name' },
        issue_number: { type: 'integer', description: 'Issue number' },
      },
      required: ['owner', 'repo', 'issue_number'],
    },
    required: ['owner', 'repo', 'issue_number'],
  },
  {
    name: 'list_pull_requests',
    description: 'List pull requests of a repository.',
    inputSchema: {
      type: 'object',
      properties: {
        owner: { type: 'string', description: 'Repository owner login' },
        repo: { type: 'string', description: 'Repository name' },
        state: { type: 'string', description: 'Pull request state: open, closed or all' },
      },
      required: ['owner', 'repo'],
    },
    required: ['owner', 'repo'],
  },
];

function repoJson(repo: Repo): Record<string, unknown> {
  return {
    id: repo.id,
    node_id: gid('Repository', repo.id),
    name: repo.name,
    full_name: `${repo.owner}/${repo.name}`,
    owner: { login: repo.owner, id: 1000, type: 'User' },
    private: repo.private,
    description: repo.description,
    default_branch: repo.defaultBranch,
  };
}

function issueJson(issue: Issue): Record<string, unknown> {
  return {
    id: issue.id,
    node_id: gid('Issue', issue.id),
    number: issue.number,
    title: issue.title,
    body: issue.body,
    state: issue.state,
    labels: issue.labels.map((label) => ({ name: label.name, color: label.color })),
  };
}

function pullJson(forge: Forge, repo: Repo, pull: Pull): Record<string, unknown> {
  return {
    id: pull.number,
    node_id: gid('PullRequest', repo.id, pull.number),
    number: pull.number,
    title: pull.title,
    body: pull.body,
    state: pull.state,
    head: { ref: pull.headRef, sha: forge.headSha(repo, pull) },
    base: { ref: pull.baseRef, sha: forge.baseSha(repo, pull) },
    merged: pull.merged,
    merged_at: pull.mergedAt,
    merge_commit_sha: pull.mergeCommitSha,
  };
}

function toolOk(value: unknown): Record<string, unknown> {
  const text = typeof value === 'string' ? value : JSON.stringify(value);
  return { content: [{ type: 'text', text }], structuredContent: value, isError: false };
}

function toolErr(message: string): Record<string, unknown> {
  return { content: [{ type: 'text', text: message }], isError: true };
}

function resolveRepo(forge: Forge, args: Record<string, unknown>): Repo | null {
  const owner = typeof args.owner === 'string' ? args.owner : '';
  const repoName =
    typeof args.repo === 'string'
      ? args.repo
      : typeof args.full_name === 'string'
        ? (args.full_name as string).split('/').slice(1).join('/')
        : '';
  if (owner === '') return null;
  const repo = forge.getRepo(owner, repoName);
  return repo ?? null;
}

function dispatchTool(forge: Forge, name: string, args: Record<string, unknown>): Record<string, unknown> {
  switch (name) {
    case 'create_repo': {
      const repoName = typeof args.name === 'string' ? args.name : '';
      if (repoName === '') return toolErr('Missing required argument: name');
      const repo = forge.createRepo(VIEWER.login, repoName, {
        private: typeof args.private === 'boolean' ? args.private : false,
        description: typeof args.description === 'string' ? args.description : '',
      });
      if (!repo) return toolErr('Repository already exists');
      return toolOk(repoJson(repo));
    }
    case 'get_repo': {
      const repo = resolveRepo(forge, args);
      if (!repo) return toolErr('Repository not found');
      return toolOk(repoJson(repo));
    }
    case 'create_issue': {
      const repo = resolveRepo(forge, args);
      if (!repo) return toolErr('Repository not found');
      const title = typeof args.title === 'string' ? args.title : '';
      if (title === '') return toolErr('Missing required argument: title');
      const created = forge.createIssue(repo, { title, body: typeof args.body === 'string' ? args.body : '' });
      if ('error' in created) return toolErr(created.error);
      return toolOk(issueJson(created));
    }
    case 'list_issues': {
      const repo = resolveRepo(forge, args);
      if (!repo) return toolErr('Repository not found');
      const state = typeof args.state === 'string' ? args.state : 'open';
      const list = forge.listIssues(repo, state, 'created', 'asc');
      return toolOk(list.map(issueJson));
    }
    case 'create_pull_request': {
      const repo = resolveRepo(forge, args);
      if (!repo) return toolErr('Repository not found');
      const created = forge.createPull(repo, {
        head: typeof args.head === 'string' ? args.head : '',
        base: typeof args.base === 'string' ? args.base : '',
        title: typeof args.title === 'string' ? args.title : '',
        body: typeof args.body === 'string' ? args.body : '',
      });
      if ('error' in created) return toolErr(created.error);
      return toolOk(pullJson(forge, repo, created));
    }
    case 'merge_pull_request': {
      const repo = resolveRepo(forge, args);
      if (!repo) return toolErr('Repository not found');
      const number = typeof args.pull_number === 'number' ? args.pull_number : Number(args.pull_number);
      const pull = forge.getPull(repo, number);
      if (!pull) return toolErr('Pull request not found');
      if (pull.merged) return toolErr('Pull request already merged');
      const merged = forge.mergePull(repo, pull);
      if (!merged) return toolErr('Pull request is conflicted');
      return toolOk({ number: pull.number, merged: true, state: pull.state, sha: merged.sha });
    }
    case 'get_issue': {
      const repo = resolveRepo(forge, args);
      if (!repo) return toolErr('Repository not found');
      const number = typeof args.issue_number === 'number' ? args.issue_number : Number(args.issue_number);
      const issue = forge.getIssue(repo, number);
      if (!issue) return toolErr('Issue not found');
      return toolOk(issueJson(issue));
    }
    case 'list_pull_requests': {
      const repo = resolveRepo(forge, args);
      if (!repo) return toolErr('Repository not found');
      const state = typeof args.state === 'string' ? args.state : 'open';
      return toolOk(forge.listPulls(repo, state).map((pull) => pullJson(forge, repo, pull)));
    }
    default:
      return toolErr(`Unknown tool: ${name}`);
  }
}

function rpcResult(res: ServerResponse, id: unknown, result: unknown): void {
  const text = JSON.stringify({ jsonrpc: '2.0', id: id ?? null, result });
  res.writeHead(200, { 'content-type': 'application/json', 'content-length': Buffer.byteLength(text) });
  res.end(text);
}

function rpcError(res: ServerResponse, status: number, id: unknown, code: number, message: string): void {
  const text = JSON.stringify({ jsonrpc: '2.0', id: id ?? null, error: { code, message } });
  res.writeHead(status, { 'content-type': 'application/json', 'content-length': Buffer.byteLength(text) });
  res.end(text);
}

export async function handleMcp(req: IncomingMessage, res: ServerResponse, ctx: Ctx): Promise<void> {
  const method = req.method ?? 'GET';
  if (method === 'DELETE') {
    res.writeHead(204);
    res.end();
    return;
  }
  if (method !== 'POST') {
    rpcError(res, 405, null, -32601, 'Method not allowed');
    return;
  }
  const raw = (await readBody(req)).toString('utf8');
  let request: Record<string, unknown>;
  try {
    const parsed: unknown = JSON.parse(raw);
    if (!isRecord(parsed)) throw new Error('not an object');
    request = parsed;
  } catch {
    // The parse-error check sends truncated JSON; answer HTTP 400, JSON-RPC -32700.
    const idMatch = /"id"\s*:\s*("[^"]*"|-?\d+)/.exec(raw);
    let id: unknown = null;
    if (idMatch) {
      const token = idMatch[1] as string;
      id = token.startsWith('"') ? token.slice(1, -1) : Number(token);
    }
    rpcError(res, 400, id, -32700, 'Parse error');
    return;
  }
  const id = request.id ?? null;
  const rpcMethod = typeof request.method === 'string' ? request.method : '';
  const params = isRecord(request.params) ? request.params : {};

  if (rpcMethod === 'server/discover') {
    const serverInfo = { name: SERVER_NAME, version: SERVER_VERSION };
    rpcResult(res, id, {
      supportedVersions: SUPPORTED_VERSIONS,
      capabilities: { tools: {} },
      serverInfo,
      _meta: { 'io.modelcontextprotocol/serverInfo': serverInfo },
    });
    return;
  }
  if (rpcMethod === 'initialize') {
    const wanted = typeof params.protocolVersion === 'string' ? params.protocolVersion : '';
    const protocolVersion = SUPPORTED_VERSIONS.includes(wanted) ? wanted : '2026-07-28';
    rpcResult(res, id, {
      protocolVersion,
      capabilities: { tools: {} },
      serverInfo: { name: SERVER_NAME, version: SERVER_VERSION },
    });
    return;
  }
  if (rpcMethod === 'notifications/initialized' || rpcMethod.startsWith('notifications/')) {
    // A notification carries no id and expects no result.
    res.writeHead(202, { 'content-type': 'application/json', 'content-length': 2 });
    res.end('{}');
    return;
  }
  if (rpcMethod === 'ping') {
    rpcResult(res, id, {});
    return;
  }
  if (rpcMethod === 'tools/list') {
    rpcResult(res, id, {
      tools: TOOLS.map((tool) => ({ name: tool.name, description: tool.description, inputSchema: tool.inputSchema })),
    });
    return;
  }
  if (rpcMethod === 'tools/call') {
    const name = typeof params.name === 'string' ? params.name : '';
    const args = isRecord(params.arguments) ? params.arguments : {};
    const tool = TOOLS.find((entry) => entry.name === name);
    if (!tool) {
      rpcError(res, 200, id, -32602, `Unknown tool: ${name}`);
      return;
    }
    const missing = tool.required.filter((key) => args[key] === undefined || args[key] === '' || args[key] === null);
    if (missing.length > 0) {
      rpcResult(res, id, toolErr(`Missing required argument: ${missing.join(', ')}`));
      return;
    }
    rpcResult(res, id, dispatchTool(ctx.forge, name, args));
    return;
  }
  rpcError(res, 200, id, -32601, `Method not found: ${rpcMethod}`);
}
