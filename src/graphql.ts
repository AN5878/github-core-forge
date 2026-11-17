// GraphQL v4 (POST /api/graphql): a small parser and executor for the documents
// gh and github-mcp-server actually send — viewer, repository fields, __type
// probes, issue/pull-request reads, the five mutations, node(id:) and batching.
// Unknown fields answer HTTP 200 with an `errors` array naming the field.

import { gid, parseGid, type Forge, type Issue, type Pull, type Repo } from './store.ts';
import { readBody } from './http.ts';
import { VIEWER } from './scenario.ts';

// ------------------------------------------------------------------ parser

type Value =
  | { kind: 'string'; value: string }
  | { kind: 'int'; value: number }
  | { kind: 'float'; value: number }
  | { kind: 'boolean'; value: boolean }
  | { kind: 'null' }
  | { kind: 'enum'; value: string }
  | { kind: 'variable'; name: string }
  | { kind: 'list'; items: Value[] }
  | { kind: 'object'; fields: Array<{ name: string; value: Value }> };

interface Field {
  alias: string | null;
  name: string;
  args: Array<{ name: string; value: Value }>;
  selection: Field[] | null;
  fragmentType: string | null;
}

interface Operation {
  kind: 'query' | 'mutation';
  name: string | null;
  variables: Record<string, Value>;
  selection: Field[];
}

class ParseError extends Error {}

class Lexer {
  pos = 0;
  text: string;

  constructor(text: string) {
    this.text = text;
  }

  skipIgnored(): void {
    for (;;) {
      while (this.pos < this.text.length && /[\s,]/.test(this.text[this.pos] as string)) this.pos += 1;
      if (this.text[this.pos] === '#') {
        while (this.pos < this.text.length && this.text[this.pos] !== '\n') this.pos += 1;
        continue;
      }
      break;
    }
  }

  peek(): string {
    this.skipIgnored();
    return this.text[this.pos] ?? '';
  }

  next(): string {
    this.skipIgnored();
    return this.text[this.pos] ?? '';
  }

  consume(expected: string): void {
    this.skipIgnored();
    if (!this.text.startsWith(expected, this.pos)) throw new ParseError(`expected ${expected} at ${this.pos}`);
    this.pos += expected.length;
  }

  tryConsume(expected: string): boolean {
    this.skipIgnored();
    if (!this.text.startsWith(expected, this.pos)) return false;
    this.pos += expected.length;
    return true;
  }

  readName(): string {
    this.skipIgnored();
    const match = /^[A-Za-z_][A-Za-z0-9_]*/.exec(this.text.slice(this.pos));
    if (!match) throw new ParseError(`expected a name at ${this.pos}`);
    this.pos += match[0].length;
    return match[0];
  }

  readString(): string {
    this.skipIgnored();
    if (this.text[this.pos] !== '"') throw new ParseError(`expected a string at ${this.pos}`);
    this.pos += 1;
    let out = '';
    for (;;) {
      if (this.pos >= this.text.length) throw new ParseError('unterminated string');
      const ch = this.text[this.pos] as string;
      if (ch === '"') {
        this.pos += 1;
        return out;
      }
      if (ch === '\\') {
        const next = this.text[this.pos + 1] as string;
        const escapes: Record<string, string> = { '"': '"', '\\': '\\', '/': '/', b: '\b', f: '\f', n: '\n', r: '\r', t: '\t' };
        if (next === 'u') {
          out += String.fromCharCode(Number.parseInt(this.text.slice(this.pos + 2, this.pos + 6), 16));
          this.pos += 6;
        } else {
          out += escapes[next] ?? next;
          this.pos += 2;
        }
        continue;
      }
      out += ch;
      this.pos += 1;
    }
  }

  readNumber(): Value {
    this.skipIgnored();
    const match = /^-?\d+(\.\d+)?([eE][+-]?\d+)?/.exec(this.text.slice(this.pos));
    if (!match) throw new ParseError(`expected a number at ${this.pos}`);
    this.pos += match[0].length;
    const raw = match[0];
    if (raw.includes('.') || /[eE]/.test(raw)) return { kind: 'float', value: Number(raw) };
    return { kind: 'int', value: Number(raw) };
  }
}

function parseValue(lexer: Lexer): Value {
  const ch = lexer.next();
  if (ch === '$') {
    lexer.consume('$');
    return { kind: 'variable', name: lexer.readName() };
  }
  if (ch === '"') return { kind: 'string', value: lexer.readString() };
  if (ch === '[') {
    lexer.consume('[');
    const items: Value[] = [];
    while (lexer.next() !== ']') items.push(parseValue(lexer));
    lexer.consume(']');
    return { kind: 'list', items };
  }
  if (ch === '{') {
    lexer.consume('{');
    const fields: Array<{ name: string; value: Value }> = [];
    while (lexer.next() !== '}') {
      const name = lexer.readName();
      lexer.consume(':');
      fields.push({ name, value: parseValue(lexer) });
    }
    lexer.consume('}');
    return { kind: 'object', fields };
  }
  if (ch === '-' || /[0-9]/.test(ch)) return lexer.readNumber();
  const name = lexer.readName();
  if (name === 'true') return { kind: 'boolean', value: true };
  if (name === 'false') return { kind: 'boolean', value: false };
  if (name === 'null') return { kind: 'null' };
  return { kind: 'enum', value: name };
}

function parseSelectionSet(lexer: Lexer): Field[] {
  lexer.consume('{');
  const fields: Field[] = [];
  while (lexer.next() !== '}') {
    if (lexer.text.startsWith('...', lexer.pos)) {
      lexer.consume('...');
      let fragmentType: string | null = null;
      if (lexer.next() !== '{') {
        const keyword = lexer.readName();
        if (keyword === 'on') fragmentType = lexer.readName();
        else throw new ParseError(`unsupported fragment spread ${keyword}`);
      }
      const inner = parseSelectionSet(lexer);
      for (const field of inner) fields.push({ ...field, fragmentType });
      continue;
    }
    const first = lexer.readName();
    let alias: string | null = null;
    let name = first;
    if (lexer.tryConsume(':')) {
      alias = first;
      name = lexer.readName();
    }
    const args: Array<{ name: string; value: Value }> = [];
    if (lexer.next() === '(') {
      lexer.consume('(');
      while (lexer.next() !== ')') {
        const argName = lexer.readName();
        lexer.consume(':');
        args.push({ name: argName, value: parseValue(lexer) });
      }
      lexer.consume(')');
    }
    const selection = lexer.next() === '{' ? parseSelectionSet(lexer) : null;
    fields.push({ alias, name, args, selection, fragmentType: null });
  }
  lexer.consume('}');
  return fields;
}

function parseOperation(text: string): Operation {
  const lexer = new Lexer(text);
  let kind: 'query' | 'mutation' = 'query';
  const first = lexer.next();
  if (first === '{') {
    return { kind: 'query', name: null, variables: {}, selection: parseSelectionSet(lexer) };
  }
  const keyword = lexer.readName();
  if (keyword !== 'query' && keyword !== 'mutation') throw new ParseError(`unsupported operation ${keyword}`);
  kind = keyword;
  let name: string | null = null;
  if (/[A-Za-z_]/.test(lexer.next())) name = lexer.readName();
  const variables: Record<string, Value> = {};
  if (lexer.next() === '(') {
    lexer.consume('(');
    while (lexer.next() !== ')') {
      lexer.consume('$');
      const varName = lexer.readName();
      lexer.consume(':');
      // Skip the type (a name, list brackets, or !) and an optional default value.
      let depth = 0;
      for (;;) {
        const ch = lexer.next();
        if (ch === '[') {
          lexer.consume('[');
          depth += 1;
        } else if (ch === ']') {
          lexer.consume(']');
          depth -= 1;
        } else if (ch === '!') {
          lexer.consume('!');
        } else if (/[A-Za-z_]/.test(ch)) {
          lexer.readName();
        } else break;
        if (depth === 0 && lexer.next() !== '[' && lexer.next() !== '!' && !/[A-Za-z_]/.test(lexer.next())) break;
      }
      if (lexer.tryConsume('=')) variables[varName] = parseValue(lexer);
      else variables[varName] = { kind: 'null' };
    }
    lexer.consume(')');
  }
  return { kind, name, variables, selection: parseSelectionSet(lexer) };
}

// ------------------------------------------------------------------ values

function evalValue(value: Value, variables: Record<string, unknown>): unknown {
  switch (value.kind) {
    case 'string':
    case 'int':
    case 'float':
    case 'boolean':
      return value.value;
    case 'null':
      return null;
    case 'enum':
      return value.value;
    case 'variable':
      return variables[value.name] ?? null;
    case 'list':
      return value.items.map((item) => evalValue(item, variables));
    case 'object': {
      const out: Record<string, unknown> = {};
      for (const field of value.fields) out[field.name] = evalValue(field.value, variables);
      return out;
    }
  }
}

function argValue(field: Field, name: string, variables: Record<string, unknown>): unknown {
  const arg = field.args.find((entry) => entry.name === name);
  if (!arg) return undefined;
  return evalValue(arg.value, variables);
}

// ------------------------------------------------------------------ schema

const TYPE_FIELDS: Record<string, string[]> = {
  Issue: ['id', 'number', 'title', 'state', 'body', 'createdAt', 'updatedAt', 'author', 'labels', 'comments', 'url', 'repository'],
  PullRequest: ['id', 'number', 'title', 'body', 'state', 'merged', 'mergeable', 'headRefName', 'baseRefName', 'headRef', 'baseRef', 'files', 'author', 'createdAt', 'updatedAt'],
  Repository: ['id', 'databaseId', 'name', 'nameWithOwner', 'owner', 'defaultBranchRef', 'viewerPermission', 'hasIssuesEnabled', 'issue', 'issues', 'pullRequests', 'pullRequest', 'description', 'private', 'url', 'createdAt'],
  SearchType: [],
  User: ['id', 'login', 'name', 'email'],
  IssueConnection: ['nodes', 'edges', 'pageInfo', 'totalCount'],
  PullRequestConnection: ['nodes', 'edges', 'pageInfo', 'totalCount'],
  PullRequestChangedFileConnection: ['nodes', 'edges', 'pageInfo', 'totalCount'],
  PullRequestChangedFile: ['path', 'additions', 'deletions', 'changes'],
  PageInfo: ['hasNextPage', 'hasPreviousPage', 'startCursor', 'endCursor'],
  Ref: ['name', 'target'],
  CommentEdge: ['node', 'cursor'],
  IssueComment: ['id', 'body', 'author', 'createdAt'],
  Query: ['viewer', 'repository', 'node', '__type', '__schema'],
  Mutation: ['createIssue', 'closeIssue', 'createPullRequest', 'mergePullRequest', 'addComment'],
};

const TYPE_KINDS: Record<string, string> = {
  SearchType: 'ENUM',
  IssueState: 'ENUM',
  PullRequestState: 'ENUM',
  MergeableState: 'ENUM',
  OrderDirection: 'ENUM',
  IssueOrderField: 'ENUM',
  RepositoryPermission: 'ENUM',
};

const ENUM_VALUES: Record<string, string[]> = {
  SearchType: ['ISSUE', 'REPOSITORY', 'USER', 'DISCUSSION', 'COMMIT', 'CODE', 'REGISTRY_PACKAGE', 'WIKI'],
  IssueState: ['OPEN', 'CLOSED'],
  PullRequestState: ['OPEN', 'CLOSED', 'MERGED'],
  MergeableState: ['MERGEABLE', 'CONFLICTING', 'UNKNOWN'],
  OrderDirection: ['ASC', 'DESC'],
  IssueOrderField: ['CREATED_AT', 'UPDATED_AT', 'COMMENTS'],
  RepositoryPermission: ['ADMIN', 'MAINTAIN', 'WRITE', 'TRIAGE', 'READ'],
};

// ------------------------------------------------------------------ executor

class GraphQLError extends Error {}

interface GqlObject {
  __typename: string;
  [key: string]: unknown;
}

function b64cursor(index: number): string {
  return Buffer.from(`cursor:${index}`, 'utf8').toString('base64url');
}

function parseCursor(cursor: unknown): number {
  if (typeof cursor !== 'string' || cursor === '') return -1;
  try {
    const text = Buffer.from(cursor, 'base64url').toString('utf8');
    const match = /^cursor:(\d+)$/.exec(text);
    return match ? Number(match[1]) : -1;
  } catch {
    return -1;
  }
}

export class GraphQLExecutor {
  forge: Forge;

  constructor(forge: Forge) {
    this.forge = forge;
  }

  execute(document: string, variables: Record<string, unknown>): { data: unknown; errors: Array<{ message: string }> } {
    let operation: Operation;
    try {
      operation = parseOperation(document);
    } catch (error) {
      return { data: null, errors: [{ message: error instanceof Error ? error.message : String(error) }] };
    }
    try {
      const root: GqlObject = { __typename: operation.kind === 'mutation' ? 'Mutation' : 'Query' };
      const data = this.resolveSelection(root, operation.selection, variables);
      return { data, errors: [] };
    } catch (error) {
      if (error instanceof GraphQLError) return { data: null, errors: [{ message: error.message }] };
      throw error;
    }
  }

  private resolveSelection(parent: GqlObject, selection: Field[], variables: Record<string, unknown>): Record<string, unknown> {
    const out: Record<string, unknown> = {};
    for (const field of selection) {
      if (field.fragmentType !== null && field.fragmentType !== parent.__typename) continue;
      const key = field.alias ?? field.name;
      const value = this.resolveField(parent, field, variables);
      if (field.selection !== null && value !== null && typeof value === 'object') {
        if (Array.isArray(value)) {
          out[key] = value.map((item) =>
            item !== null && typeof item === 'object'
              ? this.resolveSelection(item as GqlObject, field.selection as Field[], variables)
              : item,
          );
        } else {
          out[key] = this.resolveSelection(value as GqlObject, field.selection, variables);
        }
      } else {
        out[key] = value;
      }
    }
    return out;
  }

  private unknownField(parent: GqlObject, field: Field): never {
    throw new GraphQLError(`Cannot query field "${field.name}" on type "${parent.__typename}".`);
  }

  private resolveField(parent: GqlObject, field: Field, variables: Record<string, unknown>): unknown {
    // Payloads, connections and page info carry their fields as own properties.
    if (!field.name.startsWith('__') && Object.prototype.hasOwnProperty.call(parent, field.name)) {
      return parent[field.name];
    }
    const type = parent.__typename;
    if (type === 'Query') return this.resolveQuery(parent, field, variables);
    if (type === 'Mutation') return this.resolveMutation(parent, field, variables);
    if (type === 'Repository') return this.resolveRepository(parent, field, variables);
    if (type === 'Issue') return this.resolveIssue(parent, field, variables);
    if (type === 'PullRequest') return this.resolvePullRequest(parent, field, variables);
    if (type === 'User') {
      if (field.name === 'login') return parent.login ?? null;
      if (field.name === 'id') return parent.id ?? null;
      if (field.name === 'name') return parent.name ?? null;
      if (field.name === 'email') return parent.email ?? null;
      return this.unknownField(parent, field);
    }
    if (type === '__Type') {
      if (field.name === 'name') return parent.name ?? null;
      if (field.name === 'kind') return parent.kind ?? 'OBJECT';
      if (field.name === 'fields') return (parent.fields as GqlObject[]) ?? null;
      if (field.name === 'enumValues') return (parent.enumValues as GqlObject[]) ?? null;
      return this.unknownField(parent, field);
    }
    if (type === '__Field' || type === '__EnumValue') {
      if (field.name === 'name') return parent.name ?? null;
      return this.unknownField(parent, field);
    }
    if (type === 'IssueConnection' || type === 'PullRequestConnection' || type === 'PullRequestChangedFileConnection') {
      if (field.name === 'nodes') return (parent.nodes as unknown[]) ?? [];
      if (field.name === 'edges') {
        const nodes = (parent.nodes as GqlObject[]) ?? [];
        return nodes.map((node) => ({ __typename: 'Edge', node, cursor: b64cursor(nodes.indexOf(node)) }));
      }
      if (field.name === 'pageInfo') return parent.pageInfo ?? null;
      if (field.name === 'totalCount') return ((parent.nodes as unknown[]) ?? []).length;
      return this.unknownField(parent, field);
    }
    if (type === 'PageInfo') {
      if (field.name === 'hasNextPage') return parent.hasNextPage ?? false;
      if (field.name === 'hasPreviousPage') return parent.hasPreviousPage ?? false;
      if (field.name === 'endCursor') return parent.endCursor ?? null;
      if (field.name === 'startCursor') return parent.startCursor ?? null;
      return this.unknownField(parent, field);
    }
    if (type === 'Ref') {
      if (field.name === 'name') return parent.name ?? null;
      return this.unknownField(parent, field);
    }
    if (type === 'PullRequestChangedFile') {
      if (field.name === 'path') return parent.path ?? null;
      if (field.name === 'additions') return parent.additions ?? 0;
      if (field.name === 'deletions') return parent.deletions ?? 0;
      if (field.name === 'changes') return parent.changes ?? 0;
      return this.unknownField(parent, field);
    }
    if (type === 'CommentEdge') {
      if (field.name === 'node') return parent.node ?? null;
      if (field.name === 'cursor') return parent.cursor ?? null;
      return this.unknownField(parent, field);
    }
    if (type === 'IssueComment') {
      if (field.name === 'id') return parent.id ?? null;
      if (field.name === 'body') return parent.body ?? null;
      return this.unknownField(parent, field);
    }
    if (type === 'Edge') {
      if (field.name === 'node') return parent.node ?? null;
      if (field.name === 'cursor') return parent.cursor ?? null;
      return this.unknownField(parent, field);
    }
    return this.unknownField(parent, field);
  }

  private resolveQuery(parent: GqlObject, field: Field, variables: Record<string, unknown>): unknown {
    if (field.name === 'viewer') {
      return { __typename: 'User', login: VIEWER.login, id: gid('User', VIEWER.login), name: VIEWER.name, email: VIEWER.email };
    }
    if (field.name === 'repository') {
      const owner = argValue(field, 'owner', variables);
      const name = argValue(field, 'name', variables);
      if (typeof owner !== 'string' || typeof name !== 'string') return null;
      const repo = this.forge.getRepo(owner, name);
      if (!repo || owner !== VIEWER.login) return null;
      return repoObject(repo);
    }
    if (field.name === 'node') {
      const id = argValue(field, 'id', variables);
      if (typeof id !== 'string') return null;
      const parsed = parseGid(id);
      if (!parsed) return null;
      if (parsed.kind === 'Repository') {
        const repo = this.forge.getRepoById(Number(parsed.parts[0]));
        return repo ? repoObject(repo) : null;
      }
      if (parsed.kind === 'Issue') {
        const found = this.forge.getIssueById(Number(parsed.parts[0]));
        return found ? issueObject(found.repo, found.issue) : null;
      }
      if (parsed.kind === 'PullRequest') {
        const repo = this.forge.getRepoById(Number(parsed.parts[0]));
        const pull = repo?.pulls.find((entry) => entry.number === Number(parsed.parts[1]));
        return pull && repo ? pullObject(this.forge, repo, pull) : null;
      }
      return null;
    }
    if (field.name === '__type') {
      const name = argValue(field, 'name', variables);
      if (typeof name !== 'string') return null;
      const fields = TYPE_FIELDS[name];
      if (fields === undefined && ENUM_VALUES[name] === undefined) return null;
      return {
        __typename: '__Type',
        name,
        kind: TYPE_KINDS[name] ?? 'OBJECT',
        fields: (fields ?? []).map((entry) => ({ __typename: '__Field', name: entry })),
        enumValues: (ENUM_VALUES[name] ?? []).map((entry) => ({ __typename: '__EnumValue', name: entry })),
      };
    }
    if (field.name === '__schema') {
      return { __typename: '__Schema', types: Object.keys(TYPE_FIELDS).map((name) => ({ __typename: '__Type', name })) };
    }
    return this.unknownField(parent, field);
  }

  private resolveMutation(parent: GqlObject, field: Field, variables: Record<string, unknown>): unknown {
    const input = argValue(field, 'input', variables);
    const record = (input !== null && typeof input === 'object' && !Array.isArray(input) ? input : {}) as Record<string, unknown>;
    if (field.name === 'createIssue') {
      const repoId = parseGid(String(record.repositoryId ?? ''));
      const repo = repoId?.kind === 'Repository' ? this.forge.getRepoById(Number(repoId.parts[0])) : undefined;
      if (!repo) throw new GraphQLError('repository not found');
      const title = typeof record.title === 'string' ? record.title : '';
      const created = this.forge.createIssue(repo, { title, body: typeof record.body === 'string' ? record.body : '' });
      if ('error' in created) throw new GraphQLError(created.error);
      return { __typename: 'CreateIssuePayload', issue: issueObject(repo, created) };
    }
    if (field.name === 'closeIssue') {
      const issueId = parseGid(String(record.issueId ?? ''));
      const found = issueId?.kind === 'Issue' ? this.forge.getIssueById(Number(issueId.parts[0])) : undefined;
      if (!found) throw new GraphQLError('issue not found');
      const { repo, issue } = found;
      issue.state = 'closed';
      const { iso } = this.forge.clock.next();
      issue.updatedAt = iso;
      return { __typename: 'CloseIssuePayload', issue: issueObject(repo, issue) };
    }
    if (field.name === 'createPullRequest') {
      const repoId = parseGid(String(record.repositoryId ?? ''));
      const repo = repoId?.kind === 'Repository' ? this.forge.getRepoById(Number(repoId.parts[0])) : undefined;
      if (!repo) throw new GraphQLError('repository not found');
      const created = this.forge.createPull(repo, {
        head: typeof record.headRefName === 'string' ? record.headRefName : '',
        base: typeof record.baseRefName === 'string' ? record.baseRefName : '',
        title: typeof record.title === 'string' ? record.title : '',
        body: typeof record.body === 'string' ? record.body : '',
      });
      if ('error' in created) throw new GraphQLError(created.error);
      return { __typename: 'CreatePullRequestPayload', pullRequest: pullObject(this.forge, repo, created) };
    }
    if (field.name === 'mergePullRequest') {
      const pullId = parseGid(String(record.pullRequestId ?? ''));
      const repo = pullId?.kind === 'PullRequest' ? this.forge.getRepoById(Number(pullId.parts[0])) : undefined;
      const pull = repo?.pulls.find((entry) => entry.number === Number(pullId?.parts[1]));
      if (!repo || !pull) throw new GraphQLError('pull request not found');
      if (pull.merged) throw new GraphQLError('pull request already merged');
      const merged = this.forge.mergePull(repo, pull);
      if (!merged) throw new GraphQLError('pull request is conflicted');
      return { __typename: 'MergePullRequestPayload', pullRequest: pullObject(this.forge, repo, pull) };
    }
    if (field.name === 'addComment') {
      const subjectId = parseGid(String(record.subjectId ?? ''));
      const found = subjectId?.kind === 'Issue' ? this.forge.getIssueById(Number(subjectId.parts[0])) : undefined;
      if (!found) throw new GraphQLError('issue not found');
      const { repo, issue } = found;
      const bodyText = typeof record.body === 'string' ? record.body : '';
      const { iso } = this.forge.clock.next();
      const comment = { id: this.forge.nextCommentId, body: bodyText, createdAt: iso, userLogin: VIEWER.login };
      this.forge.nextCommentId += 1;
      issue.comments.push(comment);
      return {
        __typename: 'AddCommentPayload',
        commentEdge: {
          __typename: 'CommentEdge',
          node: { __typename: 'IssueComment', id: gid('IssueComment', repo.id, issue.number, comment.id), body: comment.body },
          cursor: b64cursor(issue.comments.length - 1),
        },
      };
    }
    return this.unknownField(parent, field);
  }

  private resolveRepository(parent: GqlObject, field: Field, variables: Record<string, unknown>): unknown {
    const repo = (parent as unknown as { __repo: Repo }).__repo;
    if (field.name === 'id') return parent.id ?? null;
    if (field.name === 'databaseId') return repo.id;
    if (field.name === 'name') return repo.name;
    if (field.name === 'nameWithOwner') return `${repo.owner}/${repo.name}`;
    if (field.name === 'owner') return { __typename: 'User', login: repo.owner, id: gid('User', repo.owner) };
    if (field.name === 'defaultBranchRef') {
      const tip = repo.refs.get(`refs/heads/${repo.defaultBranch}`);
      if (!tip) return null;
      return { __typename: 'Ref', name: repo.defaultBranch };
    }
    if (field.name === 'viewerPermission') return 'ADMIN';
    if (field.name === 'hasIssuesEnabled') return true;
    if (field.name === 'description') return repo.description;
    if (field.name === 'issue') {
      const number = argValue(field, 'number', variables);
      const issue = typeof number === 'number' ? this.forge.getIssue(repo, number) : undefined;
      return issue ? issueObject(repo, issue) : null;
    }
    if (field.name === 'issues') {
      const first = argValue(field, 'first', variables);
      const after = argValue(field, 'after', variables);
      const states = argValue(field, 'states', variables);
      const orderBy = argValue(field, 'orderBy', variables);
      let list = [...repo.issues];
      if (Array.isArray(states) && states.length) {
        const wanted = new Set(states.map((state) => String(state).toLowerCase()));
        list = list.filter((issue) => wanted.has(issue.state));
      }
      let direction = 1;
      if (orderBy !== null && typeof orderBy === 'object' && !Array.isArray(orderBy)) {
        const order = orderBy as Record<string, unknown>;
        if (order.direction === 'DESC') direction = -1;
      }
      list.sort((a, b) => (a.createdSeq - b.createdSeq) * direction || (a.number - b.number) * direction);
      return connection(list.map((issue) => issueObject(repo, issue)), first, after, 'IssueConnection');
    }
    if (field.name === 'pullRequests') {
      const first = argValue(field, 'first', variables);
      const after = argValue(field, 'after', variables);
      const list = repo.pulls.filter((pull) => pull.state === 'open');
      return connection(list.map((pull) => pullObject(this.forge, repo, pull)), first, after, 'PullRequestConnection');
    }
    if (field.name === 'pullRequest') {
      const number = argValue(field, 'number', variables);
      const pull = typeof number === 'number' ? this.forge.getPull(repo, number) : undefined;
      return pull ? pullObject(this.forge, repo, pull) : null;
    }
    return this.unknownField(parent, field);
  }

  private resolveIssue(parent: GqlObject, field: Field, variables: Record<string, unknown>): unknown {
    const issue = (parent as unknown as { __issue: Issue }).__issue;
    if (field.name === 'id') return parent.id ?? null;
    if (field.name === 'number') return issue.number;
    if (field.name === 'title') return issue.title;
    if (field.name === 'state') return issue.state.toUpperCase();
    if (field.name === 'body') return issue.body;
    return this.unknownField(parent, field);
  }

  private resolvePullRequest(parent: GqlObject, field: Field, variables: Record<string, unknown>): unknown {
    const pull = (parent as unknown as { __pull: Pull }).__pull;
    const repo = (parent as unknown as { __repo: Repo }).__repo;
    if (field.name === 'id') return parent.id ?? null;
    if (field.name === 'number') return pull.number;
    if (field.name === 'title') return pull.title;
    if (field.name === 'body') return pull.body;
    if (field.name === 'state') return pull.merged ? 'MERGED' : pull.state.toUpperCase();
    if (field.name === 'merged') return pull.merged;
    if (field.name === 'headRefName') return pull.headRef;
    if (field.name === 'baseRefName') return pull.baseRef;
    if (field.name === 'mergeable') return this.forge.pullConflicts(repo, pull) ? 'CONFLICTING' : 'MERGEABLE';
    if (field.name === 'files') {
      const first = argValue(field, 'first', variables);
      const files = this.forge.pullFiles(repo, pull);
      const nodes = files.map((file) => ({ __typename: 'PullRequestChangedFile', path: file.filename, additions: file.additions, deletions: file.deletions, changes: file.changes }));
      return connection(nodes, first, null, 'PullRequestChangedFileConnection');
    }
    return this.unknownField(parent, field);
  }
}

function repoObject(repo: Repo): GqlObject {
  const obj: GqlObject = { __typename: 'Repository', id: gid('Repository', repo.id) };
  (obj as unknown as { __repo: Repo }).__repo = repo;
  return obj;
}

function issueObject(repo: Repo, issue: Issue): GqlObject {
  const obj: GqlObject = { __typename: 'Issue', id: gid('Issue', issue.id) };
  (obj as unknown as { __issue: Issue }).__issue = issue;
  return obj;
}

function pullObject(forge: Forge, repo: Repo, pull: Pull): GqlObject {
  const obj: GqlObject = { __typename: 'PullRequest', id: gid('PullRequest', repo.id, pull.number) };
  (obj as unknown as { __pull: Pull }).__pull = pull;
  (obj as unknown as { __repo: Repo }).__repo = repo;
  void forge;
  return obj;
}

function connection(nodes: GqlObject[], first: unknown, after: unknown, typename: string): GqlObject {
  const start = parseCursor(after) + 1;
  const limit = typeof first === 'number' && Number.isFinite(first) ? Math.max(0, Math.trunc(first)) : nodes.length;
  const page = nodes.slice(start, start + limit);
  const hasNextPage = start + limit < nodes.length;
  const endCursor = page.length ? b64cursor(start + page.length - 1) : null;
  return {
    __typename: typename,
    nodes: page,
    pageInfo: {
      __typename: 'PageInfo',
      hasNextPage,
      hasPreviousPage: start > 0,
      startCursor: page.length ? b64cursor(start) : null,
      endCursor,
    },
  };
}

// ------------------------------------------------------------------ HTTP

/**
 * POST /api/graphql: one operation `{query, variables}` or a batch (a JSON array
 * of operations, answered in order). Auth is decided by the caller.
 */
export async function handleGraphQL(req: import('node:http').IncomingMessage, res: import('node:http').ServerResponse, ctx: { forge: Forge }): Promise<void> {
  const raw = (await readBody(req)).toString('utf8');
  let body: unknown;
  try {
    body = raw.trim() === '' ? {} : JSON.parse(raw);
  } catch {
    const text = JSON.stringify({ errors: [{ message: 'Problems parsing JSON' }] });
    res.writeHead(400, { 'content-type': 'application/json', 'content-length': Buffer.byteLength(text) });
    res.end(text);
    return;
  }
  const executor = new GraphQLExecutor(ctx.forge);
  const runOne = (operation: unknown): Record<string, unknown> => {
    if (operation === null || typeof operation !== 'object' || Array.isArray(operation)) {
      return { data: null, errors: [{ message: 'Invalid GraphQL operation' }] };
    }
    const record = operation as Record<string, unknown>;
    const query = typeof record.query === 'string' ? record.query : '';
    const variables = (record.variables !== null && typeof record.variables === 'object' && !Array.isArray(record.variables) ? record.variables : {}) as Record<string, unknown>;
    return executor.execute(query, variables);
  };
  const answer = Array.isArray(body) ? body.map(runOne) : runOne(body);
  const text = JSON.stringify(answer);
  res.writeHead(200, { 'content-type': 'application/json', 'content-length': Buffer.byteLength(text) });
  res.end(text);
}
