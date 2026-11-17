// The forge's in-memory store: repositories, refs, git objects, issues, pull
// requests, comments and labels. Everything is deterministic: numeric ids and
// timestamps come from counters seeded at the scenario epoch, never the clock.

import {
  buildCommitText,
  buildTreeFromFiles,
  collectReachable,
  findMergeBase,
  flattenTree,
  hashObject,
  isAncestor,
  isObjectId,
  parseCommit,
  parseTag,
  putObject,
  type GitObject,
  type GitObjectType,
} from './gitcore.ts';
import { DEFAULT_BRANCH, DetClock, VIEWER, type ScenarioUser } from './scenario.ts';

export interface Issue {
  id: number;
  number: number;
  title: string;
  body: string;
  state: 'open' | 'closed';
  labels: Array<{ name: string; color: string }>;
  createdSeq: number;
  createdAt: string;
  updatedAt: string;
  userLogin: string;
  comments: Array<{ id: number; body: string; createdAt: string; userLogin: string }>;
}

export interface Pull {
  number: number;
  title: string;
  body: string;
  state: 'open' | 'closed';
  headRef: string;
  baseRef: string;
  merged: boolean;
  mergedAt: string | null;
  mergeCommitSha: string | null;
  createdSeq: number;
  createdAt: string;
  userLogin: string;
}

export interface Repo {
  id: number;
  owner: string;
  name: string;
  description: string;
  private: boolean;
  defaultBranch: string;
  refs: Map<string, string>;
  objects: Map<string, GitObject>;
  labels: Map<string, { name: string; color: string }>;
  issues: Issue[];
  pulls: Pull[];
  nextIssue: number;
  nextPull: number;
}

export function repoKey(owner: string, name: string): string {
  return `${owner}/${name}`;
}

function b64url(text: string): string {
  return Buffer.from(text, 'utf8').toString('base64url');
}

/** Opaque, stable global ids (never the plain database id or number). */
export function gid(kind: string, ...parts: Array<string | number>): string {
  return b64url(`${kind}:${parts.join(':')}`);
}

export function parseGid(id: string): { kind: string; parts: string[] } | null {
  try {
    const text = Buffer.from(id, 'base64url').toString('utf8');
    const colon = text.indexOf(':');
    if (colon < 0) return null;
    return { kind: text.slice(0, colon), parts: text.slice(colon + 1).split(':') };
  } catch {
    return null;
  }
}

export class Forge {
  repos = new Map<string, Repo>();
  reposById = new Map<number, Repo>();
  nextRepoId = 1;
  nextIssueId = 1;
  nextCommentId = 1;
  clock = new DetClock();

  viewer(): ScenarioUser {
    return VIEWER;
  }

  // ------------------------------------------------------------------ repos

  createRepo(owner: string, name: string, opts: { private?: boolean; description?: string }): Repo | null {
    if (this.repos.has(repoKey(owner, name))) return null;
    const repo: Repo = {
      id: this.nextRepoId,
      owner,
      name,
      description: opts.description ?? '',
      private: opts.private ?? false,
      defaultBranch: DEFAULT_BRANCH,
      refs: new Map(),
      objects: new Map(),
      labels: new Map(),
      issues: [],
      pulls: [],
      nextIssue: 1,
      nextPull: 1,
    };
    this.nextRepoId += 1;
    this.repos.set(repoKey(owner, name), repo);
    this.reposById.set(repo.id, repo);
    return repo;
  }

  getRepo(owner: string, name: string): Repo | undefined {
    return this.repos.get(repoKey(owner, name));
  }

  getRepoById(id: number): Repo | undefined {
    return this.reposById.get(id);
  }

  deleteRepo(owner: string, name: string): boolean {
    const repo = this.repos.get(repoKey(owner, name));
    if (!repo) return false;
    this.repos.delete(repoKey(owner, name));
    this.reposById.delete(repo.id);
    return true;
  }

  listViewerRepos(owner: string): Repo[] {
    const out: Repo[] = [];
    for (const repo of this.repos.values()) if (repo.owner === owner) out.push(repo);
    out.sort((a, b) => a.id - b.id);
    return out;
  }

  // ------------------------------------------------------------------ refs

  /** Resolve a ref name, a full ref, a sha, or 'HEAD' to a commit oid, or null. */
  resolveRef(repo: Repo, ref: string): string | null {
    if (ref === 'HEAD') {
      return repo.refs.get(`refs/heads/${repo.defaultBranch}`) ?? null;
    }
    if (ref.startsWith('refs/')) {
      const oid = repo.refs.get(ref);
      return oid ?? null;
    }
    if (isObjectId(ref)) {
      return repo.objects.has(ref) ? ref : null;
    }
    const branch = repo.refs.get(`refs/heads/${ref}`);
    if (branch) return branch;
    const tag = repo.refs.get(`refs/tags/${ref}`);
    if (tag) return tag;
    return null;
  }

  /** The commit oid a ref points at, peeling annotated tags. */
  resolveCommit(repo: Repo, ref: string): string | null {
    const oid = this.resolveRef(repo, ref);
    if (!oid) return null;
    const object = repo.objects.get(oid);
    if (!object) return null;
    if (object.type === 'tag') return parseTag(object.data).object;
    if (object.type === 'commit') return oid;
    return null;
  }

  /** Commits reachable from `ref`, newest first (tip, then parents depth-first). */
  listCommits(repo: Repo, ref: string | null): Array<{ sha: string; message: string; author: string; committer: string }> {
    const tip = ref === null ? repo.refs.get(`refs/heads/${repo.defaultBranch}`) ?? null : this.resolveCommit(repo, ref);
    if (!tip) return [];
    const out: Array<{ sha: string; message: string; author: string; committer: string }> = [];
    const seen = new Set<string>();
    const stack = [tip];
    while (stack.length) {
      const oid = stack.pop() as string;
      if (seen.has(oid)) continue;
      seen.add(oid);
      const object = repo.objects.get(oid);
      if (!object || object.type !== 'commit') continue;
      const commit = parseCommit(object.data);
      out.push({ sha: oid, message: commit.message, author: commit.author, committer: commit.committer });
      for (let i = commit.parents.length - 1; i >= 0; i -= 1) stack.push(commit.parents[i] as string);
    }
    return out;
  }

  // ------------------------------------------------------------------ contents

  private identity(epochSec: number): string {
    return `${VIEWER.name} <${VIEWER.email}> ${epochSec} +0000`;
  }

  /**
   * Commit one file change on `branch` (creating the branch from the default tip
   * when it does not exist yet, or from nothing when the repo is unborn).
   * Returns the new commit oid, or null when the branch is unborn and no parent exists
   * (still fine: a root commit is built).
   */
  commitFile(
    repo: Repo,
    branch: string,
    path: string,
    content: Buffer | null,
    message: string,
  ): { commitSha: string; blobSha: string | null } {
    const tipRef = `refs/heads/${branch}`;
    const tip = repo.refs.get(tipRef) ?? repo.refs.get(`refs/heads/${repo.defaultBranch}`) ?? null;
    const { epochSec } = this.clock.next();
    let files = new Map<string, { oid: string; mode: string }>();
    const parents: string[] = [];
    if (tip !== null) {
      const tipObject = repo.objects.get(tip);
      if (tipObject && tipObject.type === 'commit') {
        parents.push(tip);
        files = flattenTree(parseCommit(tipObject.data).tree, (oid) => repo.objects.get(oid));
      }
    }
    let blobSha: string | null = null;
    if (content === null) {
      files.delete(path);
    } else {
      blobSha = putObject(repo.objects, 'blob', content);
      files.set(path, { oid: blobSha, mode: '100644' });
    }
    const treeOid = buildTreeFromFiles(files, repo.objects);
    const who = this.identity(epochSec);
    const commitData = buildCommitText(treeOid, parents, who, who, message);
    const commitSha = putObject(repo.objects, 'commit', commitData);
    repo.refs.set(tipRef, commitSha);
    return { commitSha, blobSha };
  }

  /** The file map at a commit, or null when the commit is unknown. */
  filesAt(repo: Repo, commitOid: string): Map<string, { oid: string; mode: string }> | null {
    const object = repo.objects.get(commitOid);
    if (!object || object.type !== 'commit') return null;
    return flattenTree(parseCommit(object.data).tree, (oid) => repo.objects.get(oid));
  }

  // ------------------------------------------------------------------ issues

  createIssue(repo: Repo, input: { title: string; body?: string; labels?: string[] }): Issue | { error: string } {
    const labels: Array<{ name: string; color: string }> = [];
    for (const name of input.labels ?? []) {
      const label = repo.labels.get(name);
      if (!label) return { error: `Label '${name}' does not exist` };
      labels.push({ name: label.name, color: label.color });
    }
    const { iso } = this.clock.next();
    const issue: Issue = {
      id: this.nextIssueId,
      number: repo.nextIssue,
      title: input.title,
      body: input.body ?? '',
      state: 'open',
      labels,
      createdSeq: this.clock.seq,
      createdAt: iso,
      updatedAt: iso,
      userLogin: VIEWER.login,
      comments: [],
    };
    this.nextIssueId += 1;
    repo.nextIssue += 1;
    repo.issues.push(issue);
    return issue;
  }

  getIssue(repo: Repo, number: number): Issue | undefined {
    return repo.issues.find((issue) => issue.number === number);
  }

  /** Global issue lookup: issue ids are forge-wide, so an id alone locates its repo. */
  getIssueById(id: number): { repo: Repo; issue: Issue } | undefined {
    for (const repo of this.repos.values()) {
      const issue = repo.issues.find((entry) => entry.id === id);
      if (issue) return { repo, issue };
    }
    return undefined;
  }

  listIssues(repo: Repo, state: string, sort: string, direction: string): Issue[] {
    let list = repo.issues.filter((issue) => state === 'all' || issue.state === state);
    const dir = direction === 'desc' ? -1 : 1;
    if (sort === 'created') {
      list = [...list].sort((a, b) => (a.createdSeq - b.createdSeq) * dir || (a.number - b.number) * dir);
    } else {
      list = [...list].sort((a, b) => (a.number - b.number) * dir);
    }
    return list;
  }

  // ------------------------------------------------------------------ pulls

  createPull(
    repo: Repo,
    input: { head: string; base: string; title: string; body?: string },
  ): Pull | { error: string; status: number } {
    const headTip = this.resolveCommit(repo, input.head);
    const baseTip = this.resolveCommit(repo, input.base);
    if (!headTip || !baseTip) return { error: 'Head or base branch does not exist', status: 422 };
    for (const pull of repo.pulls) {
      if (pull.state === 'open' && pull.headRef === input.head && pull.baseRef === input.base) {
        return { error: 'A pull request already exists for this head and base', status: 422 };
      }
    }
    const { iso } = this.clock.next();
    const pull: Pull = {
      number: repo.nextPull,
      title: input.title,
      body: input.body ?? '',
      state: 'open',
      headRef: input.head,
      baseRef: input.base,
      merged: false,
      mergedAt: null,
      mergeCommitSha: null,
      createdSeq: this.clock.seq,
      createdAt: iso,
      userLogin: VIEWER.login,
    };
    repo.nextPull += 1;
    repo.pulls.push(pull);
    return pull;
  }

  getPull(repo: Repo, number: number): Pull | undefined {
    return repo.pulls.find((pull) => pull.number === number);
  }

  listPulls(repo: Repo, state: string): Pull[] {
    if (state === 'all') return [...repo.pulls];
    if (state === 'closed') return repo.pulls.filter((pull) => pull.state === 'closed');
    return repo.pulls.filter((pull) => pull.state === 'open');
  }

  headSha(repo: Repo, pull: Pull): string {
    return this.resolveCommit(repo, pull.headRef) ?? '';
  }

  baseSha(repo: Repo, pull: Pull): string {
    return this.resolveCommit(repo, pull.baseRef) ?? '';
  }

  /** The changed paths of a pull request: diff of merge-base tree vs head tree. */
  pullFiles(repo: Repo, pull: Pull): Array<{ filename: string; status: string; additions: number; deletions: number; changes: number }> {
    const headTip = this.resolveCommit(repo, pull.headRef);
    const baseTip = this.resolveCommit(repo, pull.baseRef);
    if (!headTip || !baseTip) return [];
    const lookup = (oid: string): GitObject | undefined => repo.objects.get(oid);
    const mergeBase = findMergeBase(headTip, baseTip, lookup);
    const baseFiles = mergeBase ? this.filesAt(repo, mergeBase) ?? new Map() : new Map();
    const headFiles = this.filesAt(repo, headTip) ?? new Map();
    const out: Array<{ filename: string; status: string; additions: number; deletions: number; changes: number }> = [];
    const paths = new Set([...baseFiles.keys(), ...headFiles.keys()]);
    for (const path of [...paths].sort()) {
      const before = baseFiles.get(path);
      const after = headFiles.get(path);
      if (before && after && before.oid === after.oid) continue;
      let status = 'modified';
      let additions = 0;
      let deletions = 0;
      const countLines = (oid: string): string[] => {
        const object = repo.objects.get(oid);
        if (!object) return [];
        return object.data.toString('utf8').split('\n').filter((line, i, arr) => !(i === arr.length - 1 && line === ''));
      };
      if (!before && after) {
        status = 'added';
        additions = countLines(after.oid).length;
      } else if (before && !after) {
        status = 'removed';
        deletions = countLines(before.oid).length;
      } else if (before && after) {
        const beforeLines = countLines(before.oid);
        const afterLines = countLines(after.oid);
        const beforeSet = new Map<string, number>();
        for (const line of beforeLines) beforeSet.set(line, (beforeSet.get(line) ?? 0) + 1);
        const afterSet = new Map<string, number>();
        for (const line of afterLines) afterSet.set(line, (afterSet.get(line) ?? 0) + 1);
        for (const [line, count] of afterSet) {
          const used = Math.min(count, beforeSet.get(line) ?? 0);
          additions += count - used;
        }
        for (const [line, count] of beforeSet) {
          const used = Math.min(count, afterSet.get(line) ?? 0);
          deletions += count - used;
        }
      }
      out.push({ filename: path, status, additions, deletions, changes: additions + deletions });
    }
    return out;
  }

  /** Whether the pull request conflicts now (both sides changed the same path differently). */
  pullConflicts(repo: Repo, pull: Pull): boolean {
    const headTip = this.resolveCommit(repo, pull.headRef);
    const baseTip = this.resolveCommit(repo, pull.baseRef);
    if (!headTip || !baseTip) return false;
    const lookup = (oid: string): GitObject | undefined => repo.objects.get(oid);
    const mergeBase = findMergeBase(headTip, baseTip, lookup);
    if (!mergeBase) return false;
    const baseFiles = this.filesAt(repo, mergeBase) ?? new Map();
    const headFiles = this.filesAt(repo, headTip) ?? new Map();
    const currentFiles = this.filesAt(repo, baseTip) ?? new Map();
    const paths = new Set([...baseFiles.keys(), ...headFiles.keys(), ...currentFiles.keys()]);
    for (const path of paths) {
      const ancestor = baseFiles.get(path)?.oid ?? null;
      const head = headFiles.get(path)?.oid ?? null;
      const current = currentFiles.get(path)?.oid ?? null;
      const headChanged = head !== ancestor;
      const currentChanged = current !== ancestor;
      if (headChanged && currentChanged && head !== current) return true;
    }
    return false;
  }

  /**
   * Squash-merge a pull request: a 3-way merge of the head's changes onto the base
   * tip, one commit with the base tip as parent. Returns the merge commit sha, or
   * null when the pull request conflicts.
   */
  mergePull(repo: Repo, pull: Pull): { sha: string; message: string } | null {
    if (this.pullConflicts(repo, pull)) return null;
    const headTip = this.resolveCommit(repo, pull.headRef);
    const baseTip = this.resolveCommit(repo, pull.baseRef);
    if (!headTip || !baseTip) return null;
    const lookup = (oid: string): GitObject | undefined => repo.objects.get(oid);
    const mergeBase = findMergeBase(headTip, baseTip, lookup);
    const baseFiles = mergeBase ? this.filesAt(repo, mergeBase) ?? new Map() : new Map();
    const headFiles = this.filesAt(repo, headTip) ?? new Map();
    const currentFiles = this.filesAt(repo, baseTip) ?? new Map();
    const merged = new Map<string, { oid: string; mode: string }>(currentFiles);
    const paths = new Set([...baseFiles.keys(), ...headFiles.keys()]);
    for (const path of paths) {
      const ancestor = baseFiles.get(path)?.oid ?? null;
      const head = headFiles.get(path);
      const headChanged = (head?.oid ?? null) !== ancestor;
      if (!headChanged) continue;
      if (!head) merged.delete(path);
      else merged.set(path, { oid: head.oid, mode: head.mode });
    }
    const treeOid = buildTreeFromFiles(merged, repo.objects);
    const { epochSec, iso } = this.clock.next();
    const who = this.identity(epochSec);
    const message = `Merge pull request #${pull.number} from ${pull.headRef}\n\n${pull.title}`;
    const commitData = buildCommitText(treeOid, [baseTip], who, who, message);
    const mergeSha = putObject(repo.objects, 'commit', commitData);
    repo.refs.set(`refs/heads/${pull.baseRef}`, mergeSha);
    pull.state = 'closed';
    pull.merged = true;
    pull.mergedAt = iso;
    pull.mergeCommitSha = mergeSha;
    return { sha: mergeSha, message };
  }

  // ------------------------------------------------------------------ misc

  /** All objects reachable from every ref (used by the fetch pack builder). */
  reachableFrom(repo: Repo, tips: Iterable<string>): Set<string> {
    return collectReachable(tips, (oid) => repo.objects.get(oid));
  }

  /** True when `oid` is a commit in this repo. */
  isCommit(repo: Repo, oid: string): boolean {
    const object = repo.objects.get(oid);
    return object !== undefined && object.type === 'commit';
  }

  objectType(repo: Repo, oid: string): GitObjectType | null {
    return repo.objects.get(oid)?.type ?? null;
  }

  /** Peel an annotated tag to its commit, or return the oid unchanged. */
  peel(repo: Repo, oid: string): string {
    const object = repo.objects.get(oid);
    if (object && object.type === 'tag') return parseTag(object.data).object;
    return oid;
  }

  hashOf(type: GitObjectType, data: Buffer): string {
    return hashObject(type, data);
  }
}
