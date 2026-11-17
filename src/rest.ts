// REST v3 under /api/v3: repositories, branches, commits, contents, labels,
// issues, comments and pull requests. Every reply is application/json with a
// non-empty X-GitHub-Request-Id; unknown routes are JSON 404s; malformed JSON
// bodies are 400s; missing or wrong tokens are 401s.

import type { IncomingMessage, ServerResponse } from 'node:http';
import { parseCommit, parseTree } from './gitcore.ts';
import { checkAuth, isRecord, nextRequestId, pageParams, paginate, parseJsonBody, pathSegments, readBody, sendJson, type Ctx } from './http.ts';
import { VIEWER } from './scenario.ts';
import { gid, type Issue, type Pull, type Repo } from './store.ts';

function err(res: ServerResponse, ctx: Ctx, status: number, message: string): void {
  sendJson(res, status, { message }, { 'x-github-request-id': nextRequestId(ctx) });
}

function ok(res: ServerResponse, ctx: Ctx, status: number, body: unknown, extra?: Record<string, string>): void {
  sendJson(res, status, body, { 'x-github-request-id': nextRequestId(ctx), ...extra });
}

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
    has_issues: true,
    fork: false,
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
    user: { login: issue.userLogin, id: 1000, type: 'User' },
    created_at: issue.createdAt,
    updated_at: issue.updatedAt,
  };
}

function pullJson(ctx: Ctx, repo: Repo, pull: Pull): Record<string, unknown> {
  return {
    id: pull.number,
    node_id: gid('PullRequest', repo.id, pull.number),
    number: pull.number,
    title: pull.title,
    body: pull.body,
    state: pull.state,
    head: {
      ref: pull.headRef,
      sha: ctx.forge.headSha(repo, pull),
      label: `${repo.owner}:${pull.headRef}`,
      user: { login: repo.owner, id: 1000, type: 'User' },
    },
    base: {
      ref: pull.baseRef,
      sha: ctx.forge.baseSha(repo, pull),
      label: `${repo.owner}:${pull.baseRef}`,
      user: { login: repo.owner, id: 1000, type: 'User' },
    },
    merged: pull.merged,
    merged_at: pull.mergedAt,
    merge_commit_sha: pull.mergeCommitSha,
    user: { login: pull.userLogin, id: 1000, type: 'User' },
    created_at: pull.createdAt,
  };
}

function parseIdent(line: string): { name: string; email: string; date: string } {
  const match = /^(.*) <([^>]*)> (\d+) ([+-]\d{4})$/.exec(line);
  if (!match) return { name: line, email: '', date: '' };
  return {
    name: match[1] as string,
    email: match[2] as string,
    date: new Date(Number(match[3]) * 1000).toISOString(),
  };
}

export async function handleRest(req: IncomingMessage, res: ServerResponse, ctx: Ctx): Promise<void> {
  const url = new URL(req.url ?? '/', 'http://localhost');
  const method = req.method ?? 'GET';
  const segments = pathSegments(url.pathname);
  // segments[0] is 'api', segments[1] is 'v3' (the router strips those).
  const rest = segments.slice(2);
  const query = url.searchParams;

  if (!checkAuth(req, ctx)) {
    err(res, ctx, 401, 'Bad credentials');
    return;
  }

  const bodyBuf = method === 'GET' || method === 'DELETE' || method === 'HEAD' ? null : await readBody(req);
  let body: unknown = {};
  if (bodyBuf !== null) {
    const parsed = parseJsonBody(bodyBuf);
    if (!parsed.ok) {
      err(res, ctx, 400, 'Problems parsing JSON');
      return;
    }
    body = parsed.value;
  } else if (method === 'DELETE') {
    // DELETE /contents carries a JSON body; read it when present.
    const buf = await readBody(req);
    if (buf.length > 0) {
      const parsed = parseJsonBody(buf);
      if (!parsed.ok) {
        err(res, ctx, 400, 'Problems parsing JSON');
        return;
      }
      body = parsed.value;
    }
  }
  const input = isRecord(body) ? body : {};

  // ---------------------------------------------------------- /api/v3/ root
  if (rest.length === 0) {
    if (method !== 'GET') {
      err(res, ctx, 404, 'Not Found');
      return;
    }
    ok(res, ctx, 200, {
      current_user_url: '/api/v3/user',
      repository_url: '/api/v3/repos/{owner}/{repo}',
      user_url: '/api/v3/users/{user}',
    }, { 'x-oauth-scopes': 'repo, read:org' });
    return;
  }

  // ---------------------------------------------------------- /meta
  if (rest[0] === 'meta' && rest.length === 1) {
    if (method !== 'GET') {
      err(res, ctx, 404, 'Not Found');
      return;
    }
    ok(res, ctx, 200, { installed_version: '1.0.0' });
    return;
  }

  // ---------------------------------------------------------- /user
  if (rest[0] === 'user') {
    if (rest.length === 1) {
      if (method !== 'GET') {
        err(res, ctx, 404, 'Not Found');
        return;
      }
      ok(res, ctx, 200, { login: VIEWER.login, id: VIEWER.id, name: VIEWER.name, email: VIEWER.email, type: 'User' });
      return;
    }
    if (rest[1] === 'repos') {
      if (method === 'POST') {
        const name = typeof input.name === 'string' ? input.name : '';
        if (name === '') {
          err(res, ctx, 422, 'name is required');
          return;
        }
        const repo = ctx.forge.createRepo(VIEWER.login, name, {
          private: typeof input.private === 'boolean' ? input.private : false,
          description: typeof input.description === 'string' ? input.description : '',
        });
        if (!repo) {
          err(res, ctx, 422, 'Repository already exists');
          return;
        }
        ok(res, ctx, 201, repoJson(repo));
        return;
      }
      if (method === 'GET') {
        const params = pageParams(query);
        const repos = ctx.forge.listViewerRepos(VIEWER.login);
        const { page, link } = paginate(repos, params, req, url.pathname, query);
        ok(res, ctx, 200, page.map(repoJson), link === null ? undefined : { link });
        return;
      }
    }
    err(res, ctx, 404, 'Not Found');
    return;
  }

  // ---------------------------------------------------------- /repos/{owner}/{name}/...
  if (rest[0] === 'repos' && rest.length >= 3) {
    const owner = rest[1] as string;
    const name = rest[2] as string;
    const tail = rest.slice(3);
    if (owner !== VIEWER.login) {
      err(res, ctx, 404, 'Not Found');
      return;
    }
    const repo = ctx.forge.getRepo(owner, name);
    if (tail.length === 0) {
      if (!repo) {
        err(res, ctx, 404, 'Not Found');
        return;
      }
      if (method === 'GET') {
        ok(res, ctx, 200, repoJson(repo));
        return;
      }
      if (method === 'PATCH') {
        if (typeof input.description === 'string') repo.description = input.description;
        if (typeof input.private === 'boolean') repo.private = input.private;
        ok(res, ctx, 200, repoJson(repo));
        return;
      }
      if (method === 'DELETE') {
        ctx.forge.deleteRepo(owner, name);
        res.writeHead(204, { 'x-github-request-id': nextRequestId(ctx) });
        res.end();
        return;
      }
      err(res, ctx, 404, 'Not Found');
      return;
    }
    if (!repo) {
      err(res, ctx, 404, 'Not Found');
      return;
    }
    await handleRepoTail(req, res, ctx, repo, tail, query, input, method, url);
    return;
  }

  err(res, ctx, 404, 'Not Found');
}

async function handleRepoTail(
  req: IncomingMessage,
  res: ServerResponse,
  ctx: Ctx,
  repo: Repo,
  tail: string[],
  query: URLSearchParams,
  input: Record<string, unknown>,
  method: string,
  url: URL,
): Promise<void> {
  const forge = ctx.forge;
  const head = tail[0] as string;

  // ---------------------------------------------------------- branches
  if (head === 'branches') {
    if (tail.length === 1) {
      if (method !== 'GET') {
        err(res, ctx, 404, 'Not Found');
        return;
      }
      const branches: Array<Record<string, unknown>> = [];
      for (const [ref, sha] of [...repo.refs.entries()].sort()) {
        if (!ref.startsWith('refs/heads/')) continue;
        branches.push({ name: ref.slice('refs/heads/'.length), commit: { sha } });
      }
      ok(res, ctx, 200, branches);
      return;
    }
    const branch = tail.slice(1).join('/');
    const sha = repo.refs.get(`refs/heads/${branch}`);
    if (!sha) {
      err(res, ctx, 404, 'Not Found');
      return;
    }
    ok(res, ctx, 200, { name: branch, commit: { sha } });
    return;
  }

  // ---------------------------------------------------------- commits
  if (head === 'commits') {
    if (method !== 'GET') {
      err(res, ctx, 404, 'Not Found');
      return;
    }
    const params = pageParams(query);
    const commits = forge.listCommits(repo, query.get('sha'));
    const { page, link } = paginate(commits, params, req, url.pathname, query);
    const out = page.map((commit) => {
      const author = parseIdent(commit.author);
      const committer = parseIdent(commit.committer);
      return {
        sha: commit.sha,
        commit: {
          message: commit.message,
          author: { name: author.name, email: author.email, date: author.date },
          committer: { name: committer.name, email: committer.email, date: committer.date },
        },
      };
    });
    ok(res, ctx, 200, out, link === null ? undefined : { link });
    return;
  }

  // ---------------------------------------------------------- git/refs
  if (head === 'git' && tail[1] === 'refs') {
    if (method === 'POST' && tail.length === 2) {
      const ref = typeof input.ref === 'string' ? input.ref : '';
      const sha = typeof input.sha === 'string' ? input.sha : '';
      if (!ref.startsWith('refs/') || !repo.objects.has(sha)) {
        err(res, ctx, 422, 'Invalid ref or sha');
        return;
      }
      if (repo.refs.has(ref)) {
        err(res, ctx, 422, 'Reference already exists');
        return;
      }
      repo.refs.set(ref, sha);
      ok(res, ctx, 201, { ref, object: { sha } });
      return;
    }
    if (method === 'GET') {
      if (tail.length === 2) {
        const refs: Array<Record<string, unknown>> = [];
        for (const [ref, sha] of [...repo.refs.entries()].sort()) refs.push({ ref, object: { sha } });
        ok(res, ctx, 200, refs);
        return;
      }
      const ref = `refs/${tail.slice(2).join('/')}`;
      const sha = repo.refs.get(ref);
      if (!sha) {
        err(res, ctx, 404, 'Not Found');
        return;
      }
      ok(res, ctx, 200, { ref, object: { sha } });
      return;
    }
    err(res, ctx, 404, 'Not Found');
    return;
  }

  // ---------------------------------------------------------- contents
  if (head === 'contents') {
    const path = tail.slice(1).join('/');
    const refParam = query.get('ref') ?? undefined;
    const commitOid = refParam === undefined
      ? repo.refs.get(`refs/heads/${repo.defaultBranch}`) ?? null
      : forge.resolveCommit(repo, refParam);
    if (method === 'GET') {
      if (!commitOid) {
        err(res, ctx, 404, 'Not Found');
        return;
      }
      const files = forge.filesAt(repo, commitOid);
      if (!files) {
        err(res, ctx, 404, 'Not Found');
        return;
      }
      if (path === '') {
        // Root directory listing.
        const entries = dirEntries(repo, files, '');
        ok(res, ctx, 200, entries);
        return;
      }
      const file = files.get(path);
      if (file) {
        const object = repo.objects.get(file.oid);
        const data = object?.data ?? Buffer.alloc(0);
        ok(res, ctx, 200, {
          type: 'file',
          encoding: 'base64',
          content: data.toString('base64'),
          sha: file.oid,
          size: data.length,
          name: path.split('/').pop(),
          path,
        });
        return;
      }
      // A directory?
      const prefix = `${path}/`;
      let isDir = false;
      for (const key of files.keys()) if (key.startsWith(prefix)) { isDir = true; break; }
      if (isDir) {
        ok(res, ctx, 200, dirEntries(repo, files, path));
        return;
      }
      err(res, ctx, 404, 'Not Found');
      return;
    }
    if (method === 'PUT') {
      const message = typeof input.message === 'string' ? input.message : '';
      const contentB64 = typeof input.content === 'string' ? input.content : '';
      if (message === '' || contentB64 === '') {
        err(res, ctx, 422, 'message and content are required');
        return;
      }
      const branch = typeof input.branch === 'string' ? input.branch : repo.defaultBranch;
      const branchTip = repo.refs.get(`refs/heads/${branch}`);
      const branchFiles = branchTip ? forge.filesAt(repo, branchTip) : null;
      const current = branchFiles?.get(path);
      const sha = typeof input.sha === 'string' ? input.sha : undefined;
      if (current) {
        if (sha === undefined) {
          err(res, ctx, 422, 'sha must be provided to update a file');
          return;
        }
        if (sha !== current.oid) {
          err(res, ctx, 409, 'sha does not match the current file');
          return;
        }
      } else if (sha !== undefined) {
        err(res, ctx, 422, 'sha does not match any existing file');
        return;
      }
      const content = Buffer.from(contentB64, 'base64');
      const { commitSha, blobSha } = forge.commitFile(repo, branch, path, content, message);
      ok(res, ctx, current ? 200 : 201, {
        content: { name: path.split('/').pop(), path, sha: blobSha, size: content.length, encoding: 'base64', type: 'file' },
        commit: { sha: commitSha, message },
      });
      return;
    }
    if (method === 'DELETE') {
      const message = typeof input.message === 'string' ? input.message : '';
      const branch = typeof input.branch === 'string' ? input.branch : repo.defaultBranch;
      const branchTip = repo.refs.get(`refs/heads/${branch}`);
      const branchFiles = branchTip ? forge.filesAt(repo, branchTip) : null;
      const current = branchFiles?.get(path);
      if (!current) {
        err(res, ctx, 404, 'Not Found');
        return;
      }
      const sha = typeof input.sha === 'string' ? input.sha : undefined;
      if (sha !== undefined && sha !== current.oid) {
        err(res, ctx, 409, 'sha does not match the current file');
        return;
      }
      const { commitSha } = forge.commitFile(repo, branch, path, null, message === '' ? 'delete' : message);
      ok(res, ctx, 200, { commit: { sha: commitSha } });
      return;
    }
    err(res, ctx, 404, 'Not Found');
    return;
  }

  // ---------------------------------------------------------- labels
  if (head === 'labels') {
    if (method === 'POST') {
      const name = typeof input.name === 'string' ? input.name : '';
      const color = typeof input.color === 'string' ? input.color : 'ededed';
      if (name === '') {
        err(res, ctx, 422, 'name is required');
        return;
      }
      if (repo.labels.has(name)) {
        err(res, ctx, 422, 'Label already exists');
        return;
      }
      repo.labels.set(name, { name, color });
      ok(res, ctx, 201, { name, color });
      return;
    }
    if (method === 'GET') {
      ok(res, ctx, 200, [...repo.labels.values()]);
      return;
    }
    err(res, ctx, 404, 'Not Found');
    return;
  }

  // ---------------------------------------------------------- issues
  if (head === 'issues') {
    if (tail.length === 1) {
      if (method === 'POST') {
        const title = typeof input.title === 'string' ? input.title : '';
        if (title === '') {
          err(res, ctx, 422, 'title is required');
          return;
        }
        const labels = Array.isArray(input.labels) ? input.labels.filter((entry): entry is string => typeof entry === 'string') : [];
        const created = forge.createIssue(repo, { title, body: typeof input.body === 'string' ? input.body : '', labels });
        if ('error' in created) {
          err(res, ctx, 422, created.error);
          return;
        }
        ok(res, ctx, 201, issueJson(created));
        return;
      }
      if (method === 'GET') {
        const state = query.get('state') ?? 'open';
        const sort = query.get('sort') ?? 'created';
        const direction = query.get('direction') ?? 'desc';
        const params = pageParams(query);
        const issues = forge.listIssues(repo, state, sort, direction);
        const { page, link } = paginate(issues, params, req, url.pathname, query);
        ok(res, ctx, 200, page.map(issueJson), link === null ? undefined : { link });
        return;
      }
      err(res, ctx, 404, 'Not Found');
      return;
    }
    const number = Number.parseInt(tail[1] as string, 10);
    if (!Number.isInteger(number)) {
      err(res, ctx, 404, 'Not Found');
      return;
    }
    const issue = forge.getIssue(repo, number);
    if (tail.length === 2) {
      if (!issue) {
        err(res, ctx, 404, 'Not Found');
        return;
      }
      if (method === 'GET') {
        ok(res, ctx, 200, issueJson(issue));
        return;
      }
      if (method === 'PATCH') {
        if (input.state === 'open' || input.state === 'closed') {
          issue.state = input.state;
          const { iso } = forge.clock.next();
          issue.updatedAt = iso;
        }
        if (typeof input.title === 'string') issue.title = input.title;
        if (typeof input.body === 'string') issue.body = input.body;
        ok(res, ctx, 200, issueJson(issue));
        return;
      }
      err(res, ctx, 404, 'Not Found');
      return;
    }
    if (tail[2] === 'comments') {
      if (!issue) {
        err(res, ctx, 404, 'Not Found');
        return;
      }
      if (method === 'POST') {
        const bodyText = typeof input.body === 'string' ? input.body : '';
        if (bodyText === '') {
          err(res, ctx, 422, 'body is required');
          return;
        }
        const { iso } = forge.clock.next();
        const comment = { id: forge.nextCommentId, body: bodyText, createdAt: iso, userLogin: VIEWER.login };
        forge.nextCommentId += 1;
        issue.comments.push(comment);
        ok(res, ctx, 201, { id: comment.id, body: comment.body, created_at: comment.createdAt, user: { login: comment.userLogin, id: 1000, type: 'User' } });
        return;
      }
      if (method === 'GET') {
        ok(res, ctx, 200, issue.comments.map((comment) => ({
          id: comment.id,
          body: comment.body,
          created_at: comment.createdAt,
          user: { login: comment.userLogin, id: 1000, type: 'User' },
        })));
        return;
      }
    }
    err(res, ctx, 404, 'Not Found');
    return;
  }

  // ---------------------------------------------------------- pulls
  if (head === 'pulls') {
    if (tail.length === 1) {
      if (method === 'POST') {
        const title = typeof input.title === 'string' ? input.title : '';
        const headRef = typeof input.head === 'string' ? input.head : '';
        const baseRef = typeof input.base === 'string' ? input.base : '';
        if (title === '' || headRef === '' || baseRef === '') {
          err(res, ctx, 422, 'title, head and base are required');
          return;
        }
        const created = forge.createPull(repo, { head: headRef, base: baseRef, title, body: typeof input.body === 'string' ? input.body : '' });
        if ('error' in created) {
          err(res, ctx, created.status, created.error);
          return;
        }
        ok(res, ctx, 201, pullJson(ctx, repo, created));
        return;
      }
      if (method === 'GET') {
        const state = query.get('state') ?? 'open';
        const params = pageParams(query);
        const pulls = forge.listPulls(repo, state);
        const { page, link } = paginate(pulls, params, req, url.pathname, query);
        ok(res, ctx, 200, page.map((pull) => pullJson(ctx, repo, pull)), link === null ? undefined : { link });
        return;
      }
      err(res, ctx, 404, 'Not Found');
      return;
    }
    const number = Number.parseInt(tail[1] as string, 10);
    if (!Number.isInteger(number)) {
      err(res, ctx, 404, 'Not Found');
      return;
    }
    const pull = forge.getPull(repo, number);
    if (tail.length === 2) {
      if (!pull) {
        err(res, ctx, 404, 'Not Found');
        return;
      }
      if (method === 'GET') {
        ok(res, ctx, 200, pullJson(ctx, repo, pull));
        return;
      }
      err(res, ctx, 404, 'Not Found');
      return;
    }
    if (tail[2] === 'files') {
      if (!pull) {
        err(res, ctx, 404, 'Not Found');
        return;
      }
      if (method === 'GET') {
        ok(res, ctx, 200, forge.pullFiles(repo, pull));
        return;
      }
      err(res, ctx, 404, 'Not Found');
      return;
    }
    if (tail[2] === 'merge') {
      if (!pull) {
        err(res, ctx, 404, 'Not Found');
        return;
      }
      if (method === 'PUT') {
        if (pull.merged) {
          err(res, ctx, 409, 'Pull request already merged');
          return;
        }
        if (pull.state === 'closed') {
          err(res, ctx, 409, 'Pull request is closed');
          return;
        }
        const merged = forge.mergePull(repo, pull);
        if (!merged) {
          err(res, ctx, 405, 'Pull request is conflicted');
          return;
        }
        ok(res, ctx, 200, { merged: true, sha: merged.sha, message: merged.message });
        return;
      }
      err(res, ctx, 404, 'Not Found');
      return;
    }
    err(res, ctx, 404, 'Not Found');
    return;
  }

  err(res, ctx, 404, 'Not Found');
}

/** Directory listing entries for `dir` ('' for the root) from a flat file map. */
function dirEntries(
  repo: Repo,
  files: Map<string, { oid: string; mode: string }>,
  dir: string,
): Array<Record<string, unknown>> {
  const prefix = dir === '' ? '' : `${dir}/`;
  const seen = new Map<string, { type: string; oid: string; size: number }>();
  for (const [path, entry] of files) {
    if (!path.startsWith(prefix)) continue;
    const rest = path.slice(prefix.length);
    const slash = rest.indexOf('/');
    if (slash < 0) {
      const object = repo.objects.get(entry.oid);
      seen.set(rest, { type: 'file', oid: entry.oid, size: object?.data.length ?? 0 });
    } else {
      const name = rest.slice(0, slash);
      if (!seen.has(name)) {
        // The sub-tree's oid: find it by rebuilding is wasteful; walk the tree object.
        seen.set(name, { type: 'dir', oid: dirTreeOid(repo, dir === '' ? name : `${dir}/${name}`), size: 0 });
      }
    }
  }
  const out: Array<Record<string, unknown>> = [];
  for (const [name, entry] of [...seen.entries()].sort((a, b) => (a[0] < b[0] ? -1 : 1))) {
    out.push({
      type: entry.type,
      name,
      path: prefix + name,
      sha: entry.oid,
      ...(entry.type === 'file' ? { size: entry.size } : {}),
    });
  }
  return out;
}

/** The tree oid of a directory path at the repo's default tip ('' for the root). */
function dirTreeOid(repo: Repo, dir: string): string {
  const tip = repo.refs.get(`refs/heads/${repo.defaultBranch}`);
  if (!tip) return '';
  const object = repo.objects.get(tip);
  if (!object || object.type !== 'commit') return '';
  let treeOid = parseCommit(object.data).tree;
  if (dir === '') return treeOid;
  for (const segment of dir.split('/')) {
    const tree = repo.objects.get(treeOid);
    if (!tree || tree.type !== 'tree') return '';
    const entry = parseTree(tree.data).find((candidate) => candidate.name === segment && candidate.mode === '40000');
    if (!entry) return '';
    treeOid = entry.oid;
  }
  return treeOid;
}
