// git smart HTTP: info/refs (v2 and v0 advertisements), git-upload-pack
// (protocol v2 ls-refs/fetch, v0 fetch) and git-receive-pack (v0, report-status
// with side-band-64k). Packfiles are plain objects (no deltas); the trailer
// checksum is SHA-1 over the pack bytes.

import type { IncomingMessage, ServerResponse } from 'node:http';
import {
  collectReachable,
  decodePktLines,
  encodePktLine,
  encodePktLines,
  hashObject,
  isAncestor,
  isObjectId,
  parseCommit,
  parseTag,
  parseTree,
  readPack,
  writePack,
  ZERO_OID,
  PKT_DELIM,
  PKT_FLUSH,
  type GitObject,
} from './gitcore.ts';
import { checkAuth, readBody, type Ctx } from './http.ts';
import type { Repo } from './store.ts';

const AGENT = 'forge/1.0';

function sendGit(res: ServerResponse, status: number, body: Buffer, contentType: string): void {
  res.writeHead(status, { 'content-type': contentType, 'content-length': body.length });
  res.end(body);
}

function notFound(res: ServerResponse): void {
  const text = JSON.stringify({ message: 'Not Found' });
  res.writeHead(404, { 'content-type': 'application/json', 'content-length': Buffer.byteLength(text) });
  res.end(text);
}

function badRequest(res: ServerResponse, message: string): void {
  const text = JSON.stringify({ message });
  res.writeHead(400, { 'content-type': 'application/json', 'content-length': Buffer.byteLength(text) });
  res.end(text);
}

interface RefLine {
  name: string;
  oid: string;
  peeled?: string;
}

/** All refs of a repo, sorted, with peeled targets for annotated tags. */
function listRefs(repo: Repo): RefLine[] {
  const out: RefLine[] = [];
  for (const name of [...repo.refs.keys()].sort()) {
    const oid = repo.refs.get(name) as string;
    const entry: RefLine = { name, oid };
    if (name.startsWith('refs/tags/')) {
      const object = repo.objects.get(oid);
      if (object && object.type === 'tag') entry.peeled = parseTag(object.data).object;
    }
    out.push(entry);
  }
  return out;
}

function headOid(repo: Repo): string {
  return repo.refs.get(`refs/heads/${repo.defaultBranch}`) ?? ZERO_OID;
}

/** v0 advertisement body for a service, with the `# service=` preamble. */
function v0Advertisement(repo: Repo, service: 'git-upload-pack' | 'git-receive-pack'): Buffer {
  const defaultBranch = repo.defaultBranch;
  const caps =
    service === 'git-receive-pack'
      ? `report-status side-band-64k delete-refs symref=HEAD:refs/heads/${defaultBranch} agent=${AGENT} ofs-delta`
      : `side-band-64k symref=HEAD:refs/heads/${defaultBranch} agent=${AGENT} shallow ofs-delta thin-pack`;
  const parts: Buffer[] = [encodePktLine(`# service=${service}\n`), PKT_FLUSH];
  const refs = listRefs(repo);
  if (refs.length === 0) {
    parts.push(encodePktLine(`${ZERO_OID} capabilities^{}\u0000${caps}\n`));
  } else {
    parts.push(encodePktLine(`${headOid(repo)} HEAD\u0000${caps}\n`));
    for (const ref of refs) {
      parts.push(encodePktLine(`${ref.oid} ${ref.name}\n`));
      if (ref.peeled) parts.push(encodePktLine(`${ref.peeled} ${ref.name}^{}\n`));
    }
  }
  parts.push(PKT_FLUSH);
  return Buffer.concat(parts);
}

/** v2 capability advertisement body for upload-pack. */
function v2Advertisement(): Buffer {
  const parts: Buffer[] = [
    encodePktLine('# service=git-upload-pack\n'),
    PKT_FLUSH,
    encodePktLine('version 2\n'),
    encodePktLine(`agent=${AGENT}\n`),
    encodePktLine('ls-refs=unborn\n'),
    encodePktLine('fetch=shallow filter\n'),
    encodePktLine('object-format=sha1\n'),
    PKT_FLUSH,
  ];
  return Buffer.concat(parts);
}

/** A sideband packet stream: band-1 chunks of `data` (<= 65515 bytes each). */
function sidebandPackets(data: Buffer): Buffer[] {
  const out: Buffer[] = [];
  const CHUNK = 65515;
  if (data.length === 0) {
    out.push(encodePktLine(Buffer.concat([Buffer.from([1]), Buffer.alloc(0)])));
    return out;
  }
  for (let offset = 0; offset < data.length; offset += CHUNK) {
    const chunk = data.subarray(offset, Math.min(data.length, offset + CHUNK));
    out.push(encodePktLine(Buffer.concat([Buffer.from([1]), chunk])));
  }
  return out;
}

export async function handleGit(req: IncomingMessage, res: ServerResponse, ctx: Ctx): Promise<void> {
  const url = new URL(req.url ?? '/', 'http://localhost');
  const method = req.method ?? 'GET';
  const match = /^\/([^/]+)\/([^/]+)\.git\/(.*)$/.exec(url.pathname);
  if (!match) {
    notFound(res);
    return;
  }
  const owner = decodeURIComponent(match[1] as string);
  const repoName = decodeURIComponent(match[2] as string);
  const suffix = match[3] as string;

  if (!checkAuth(req, ctx)) {
    const text = JSON.stringify({ message: 'Unauthorized' });
    res.writeHead(401, { 'content-type': 'application/json', 'content-length': Buffer.byteLength(text) });
    res.end(text);
    return;
  }

  const repo = ctx.forge.getRepo(owner, repoName);
  if (!repo) {
    notFound(res);
    return;
  }

  if (method === 'GET' && suffix === 'info/refs') {
    const service = url.searchParams.get('service');
    if (service === 'git-upload-pack') {
      const v2 = (req.headers['git-protocol'] ?? '') === 'version=2';
      sendGit(res, 200, v2 ? v2Advertisement() : v0Advertisement(repo, 'git-upload-pack'), 'application/x-git-upload-pack-advertisement');
      return;
    }
    if (service === 'git-receive-pack') {
      sendGit(res, 200, v0Advertisement(repo, 'git-receive-pack'), 'application/x-git-receive-pack-advertisement');
      return;
    }
    notFound(res);
    return;
  }

  if (method === 'POST' && suffix === 'git-upload-pack') {
    const body = await readBody(req);
    try {
      if ((req.headers['git-protocol'] ?? '') === 'version=2') await handleUploadPackV2(req, res, ctx, repo, body);
      else await handleUploadPackV0(req, res, ctx, repo, body);
    } catch {
      badRequest(res, 'malformed request');
    }
    return;
  }

  if (method === 'POST' && suffix === 'git-receive-pack') {
    const body = await readBody(req);
    try {
      await handleReceivePack(req, res, ctx, repo, body);
    } catch {
      badRequest(res, 'malformed request');
    }
    return;
  }

  notFound(res);
}

// ------------------------------------------------------------------ ls-refs / fetch (v2)

interface FetchRequest {
  wants: string[];
  haves: string[];
  shallow: string[];
  depth: number | null;
  filter: string | null;
}

function parseV2Command(body: Buffer): { command: string; args: string[] } {
  const packets = decodePktLines(body);
  let command = '';
  const args: string[] = [];
  let afterDelim = false;
  for (const packet of packets) {
    if (packet.kind === 'delim') {
      afterDelim = true;
      continue;
    }
    if (packet.kind === 'flush') break;
    if (packet.kind !== 'data') continue;
    const line = packet.payload.toString('utf8').replace(/\n$/, '');
    if (!afterDelim) {
      if (line.startsWith('command=')) command = line.slice('command='.length);
    } else {
      args.push(line);
    }
  }
  return { command, args };
}

function lsRefsResponse(repo: Repo, args: string[]): Buffer {
  const peel = args.includes('peel');
  const symrefs = args.includes('symrefs');
  const unborn = args.includes('unborn');
  const prefixes = args.filter((arg) => arg.startsWith('ref-prefix ')).map((arg) => arg.slice('ref-prefix '.length));
  const keep = (name: string): boolean => prefixes.length === 0 || prefixes.some((prefix) => name.startsWith(prefix));
  const lines: string[] = [];
  const defaultBranch = repo.defaultBranch;
  const head = headOid(repo);
  if (prefixes.length === 0) {
    if (head !== ZERO_OID) {
      lines.push(`${head} HEAD${symrefs ? ` symref-target:refs/heads/${defaultBranch}` : ''}\n`);
    } else if (unborn) {
      lines.push(`unborn HEAD${symrefs ? ` symref-target:refs/heads/${defaultBranch}` : ''}\n`);
    }
  }
  for (const ref of listRefs(repo)) {
    if (!keep(ref.name)) continue;
    let line = `${ref.oid} ${ref.name}`;
    if (peel && ref.peeled) line += ` peeled:${ref.peeled}`;
    lines.push(`${line}\n`);
  }
  const parts = lines.map((line) => encodePktLine(line));
  parts.push(PKT_FLUSH);
  return Buffer.concat(parts);
}

/**
 * The fetch computation shared by v2 and v0: which objects to send, the new
 * shallow boundary and the unshallowed commits, and the common haves to ACK.
 */
function computeFetch(repo: Repo, request: FetchRequest): {
  objects: GitObject[];
  shallow: string[];
  unshallow: string[];
  acks: string[];
} {
  const lookup = (oid: string): GitObject | undefined => repo.objects.get(oid);
  const wants = request.wants.filter((oid) => isObjectId(oid));
  const depth = request.depth;

  // Generations from the wants (0 = the wants themselves). Index-based queue:
  // shift() is O(n) and this walk visits the whole history on a full fetch.
  const gen = new Map<string, number>();
  const queue: Array<[string, number]> = wants.map((oid) => [oid, 0] as [string, number]);
  let queueHead = 0;
  const sentCommits = new Set<string>();
  while (queueHead < queue.length) {
    const [oid, g] = queue[queueHead] as [string, number];
    queueHead += 1;
    if (gen.has(oid)) continue;
    const object = lookup(oid);
    if (!object) continue;
    if (object.type === 'tag') {
      gen.set(oid, g);
      const target = parseTag(object.data).object;
      queue.push([target, g]);
      continue;
    }
    if (object.type !== 'commit') {
      gen.set(oid, g);
      continue;
    }
    gen.set(oid, g);
    if (depth !== null && g >= depth) continue;
    sentCommits.add(oid);
    for (const parent of parseCommit(object.data).parents) queue.push([parent, g + 1]);
  }

  // Objects the client already holds: the haves' closure, stopping at the
  // known shallow boundaries (the client does not hold past them).
  const boundary = new Set(request.shallow);
  const held = new Set<string>();
  {
    const heldQueue = [...request.haves];
    while (heldQueue.length) {
      const oid = heldQueue.pop() as string;
      if (held.has(oid)) continue;
      const object = lookup(oid);
      if (!object) continue;
      held.add(oid);
      if (object.type === 'commit') {
        const commit = parseCommit(object.data);
        heldQueue.push(commit.tree);
        if (!boundary.has(oid)) for (const parent of commit.parents) heldQueue.push(parent);
      } else if (object.type === 'tag') {
        heldQueue.push(parseTag(object.data).object);
      } else if (object.type === 'tree') {
        for (const entry of parseTree(object.data)) heldQueue.push(entry.oid);
      }
    }
  }

  // Everything reachable from the sent commits, minus what the client holds.
  // The depth cut applies per commit: a commit at generation >= depth is never
  // sent, and neither is anything reachable only through it — a sent commit
  // contributes itself plus its tree closure only.
  const wanted = new Set<string>();
  for (const [oid, g] of gen) {
    if (depth !== null && g >= depth) continue;
    const object = lookup(oid);
    if (!object) continue;
    if (object.type === 'commit') {
      if (!sentCommits.has(oid)) continue;
      wanted.add(oid);
      for (const reachable of collectReachable([parseCommit(object.data).tree], lookup)) wanted.add(reachable);
    } else if (object.type === 'tag') {
      // A tag is sent itself; its target is a gen entry of its own.
      wanted.add(oid);
    } else {
      for (const reachable of collectReachable([oid], lookup)) wanted.add(reachable);
    }
  }
  const objects: Array<GitObject & { oid: string }> = [];
  for (const oid of wanted) {
    if (held.has(oid)) continue;
    const object = lookup(oid);
    if (object) objects.push({ ...object, oid });
  }

  // Shallow boundary: sent commits with an unsent parent (or at the depth cut).
  const shallow: string[] = [];
  const unshallow: string[] = [];
  if (depth !== null) {
    for (const oid of sentCommits) {
      const object = lookup(oid);
      if (!object || object.type !== 'commit') continue;
      const parents = parseCommit(object.data).parents;
      const hasUnsentParent = parents.some((parent) => !sentCommits.has(parent));
      if (hasUnsentParent) shallow.push(oid);
    }
    for (const oid of request.shallow) {
      if (sentCommits.has(oid) && !shallow.includes(oid)) unshallow.push(oid);
    }
  } else if (request.shallow.length > 0) {
    // A full fetch past a shallow boundary unshallows everything now included.
    for (const oid of request.shallow) if (sentCommits.has(oid)) unshallow.push(oid);
  }

  // ACKs: haves that are ancestors of (or equal to) a want.
  const acks: string[] = [];
  for (const have of request.haves) {
    if (!isObjectId(have)) continue;
    for (const want of wants) {
      if (isAncestor(have, want, lookup)) {
        acks.push(have);
        break;
      }
    }
  }
  return { objects, shallow, unshallow, acks };
}

/**
 * Fetch-result cache. Identical upload-pack requests (same wants, haves,
 * shallow set, depth and filter) on an unchanged object store return the
 * same pack bytes, so the bench's 50 concurrent shallow clones of one tip
 * pay the tree walk and the per-object deflate once instead of 50 times.
 * The store size in the cache slot invalidates it: any object added to the
 * store changes the size, so a cached pack can never be stale. The cache
 * holds only derived bytes, never state, so live and replay answers agree.
 */
interface CachedFetch {
  pack: Buffer;
  shallow: string[];
  unshallow: string[];
  acks: string[];
}

const fetchCache = new WeakMap<Repo, { size: number; entries: Map<string, CachedFetch> }>();

function fetchPack(repo: Repo, request: FetchRequest): CachedFetch {
  const key = JSON.stringify([request.wants, request.haves, request.shallow, request.depth, request.filter]);
  let slot = fetchCache.get(repo);
  if (slot === undefined || slot.size !== repo.objects.size) {
    slot = { size: repo.objects.size, entries: new Map<string, CachedFetch>() };
    fetchCache.set(repo, slot);
  }
  const hit = slot.entries.get(key);
  if (hit !== undefined) return hit;
  const { objects, shallow, unshallow, acks } = computeFetch(repo, request);
  const entry: CachedFetch = { pack: writePack(objects), shallow, unshallow, acks };
  slot.entries.set(key, entry);
  return entry;
}

async function handleUploadPackV2(req: IncomingMessage, res: ServerResponse, ctx: Ctx, repo: Repo, body: Buffer): Promise<void> {
  const { command, args } = parseV2Command(body);
  if (command === 'ls-refs') {
    sendGit(res, 200, lsRefsResponse(repo, args), 'application/x-git-upload-pack-result');
    return;
  }
  if (command !== 'fetch') {
    badRequest(res, `unknown command ${command}`);
    return;
  }
  const request: FetchRequest = { wants: [], haves: [], shallow: [], depth: null, filter: null };
  for (const arg of args) {
    if (arg.startsWith('want ')) request.wants.push(arg.slice(5).trim());
    else if (arg.startsWith('have ')) request.haves.push(arg.slice(5).trim());
    else if (arg.startsWith('shallow ')) request.shallow.push(arg.slice(8).trim());
    else if (arg.startsWith('deepen ')) request.depth = Number.parseInt(arg.slice(7), 10);
    else if (arg.startsWith('filter ')) request.filter = arg.slice(7);
  }
  const { pack, shallow, unshallow, acks } = fetchPack(repo, request);
  const parts: Buffer[] = [];
  parts.push(encodePktLine('acknowledgments\n'));
  if (acks.length) {
    for (const ack of acks) parts.push(encodePktLine(`ACK ${ack}\n`));
    parts.push(encodePktLine('ready\n'));
  } else {
    parts.push(encodePktLine('NAK\n'));
  }
  parts.push(PKT_DELIM);
  if (shallow.length || unshallow.length) {
    parts.push(encodePktLine('shallow-info\n'));
    for (const oid of shallow) parts.push(encodePktLine(`shallow ${oid}\n`));
    for (const oid of unshallow) parts.push(encodePktLine(`unshallow ${oid}\n`));
    parts.push(PKT_DELIM);
  }
  // listRefs once, indexed by oid: rebuilding it per want is O(wants x refs).
  const wantedRefs: Array<{ oid: string; ref: string }> = [];
  const refsByOid = new Map<string, string[]>();
  for (const ref of listRefs(repo)) {
    const names = refsByOid.get(ref.oid) ?? [];
    names.push(ref.name);
    refsByOid.set(ref.oid, names);
    if (ref.peeled !== undefined) {
      const peeledNames = refsByOid.get(ref.peeled) ?? [];
      peeledNames.push(ref.name);
      refsByOid.set(ref.peeled, peeledNames);
    }
  }
  for (const want of request.wants) {
    for (const name of refsByOid.get(want) ?? []) wantedRefs.push({ oid: want, ref: name });
  }
  if (wantedRefs.length) {
    parts.push(encodePktLine('wanted-refs\n'));
    for (const entry of wantedRefs) parts.push(encodePktLine(`${entry.oid} ${entry.ref}\n`));
    parts.push(PKT_DELIM);
  }
  parts.push(encodePktLine('packfile\n'));
  for (const packet of sidebandPackets(pack)) parts.push(packet);
  parts.push(PKT_FLUSH);
  sendGit(res, 200, Buffer.concat(parts), 'application/x-git-upload-pack-result');
}

// ------------------------------------------------------------------ fetch (v0)

async function handleUploadPackV0(req: IncomingMessage, res: ServerResponse, ctx: Ctx, repo: Repo, body: Buffer): Promise<void> {
  const packets = decodePktLines(body);
  const request: FetchRequest = { wants: [], haves: [], shallow: [], depth: null, filter: null };
  let section = 0;
  for (const packet of packets) {
    if (packet.kind === 'flush') {
      section += 1;
      continue;
    }
    if (packet.kind !== 'data') continue;
    const line = packet.payload.toString('utf8').replace(/\n$/, '');
    if (line.startsWith('want ')) {
      const rest = line.slice(5);
      const space = rest.indexOf(' ');
      request.wants.push(space < 0 ? rest : rest.slice(0, space));
    } else if (line.startsWith('have ')) request.haves.push(line.slice(5).trim());
    else if (line.startsWith('shallow ')) request.shallow.push(line.slice(8).trim());
    else if (line.startsWith('deepen ')) request.depth = Number.parseInt(line.slice(7), 10);
    else if (line === 'done') { /* end of negotiation */ }
  }
  const { pack, shallow, unshallow, acks } = fetchPack(repo, request);
  const parts: Buffer[] = [];
  if (acks.length) {
    for (const ack of acks) parts.push(encodePktLine(`ACK ${ack}\n`));
  } else {
    parts.push(encodePktLine('NAK\n'));
  }
  for (const oid of shallow) parts.push(encodePktLine(`shallow ${oid}\n`));
  for (const oid of unshallow) parts.push(encodePktLine(`unshallow ${oid}\n`));
  for (const packet of sidebandPackets(pack)) parts.push(packet);
  parts.push(PKT_FLUSH);
  sendGit(res, 200, Buffer.concat(parts), 'application/x-git-upload-pack-result');
}

// ------------------------------------------------------------------ receive-pack (v0)

async function handleReceivePack(req: IncomingMessage, res: ServerResponse, ctx: Ctx, repo: Repo, body: Buffer): Promise<void> {
  // Parse pkt-lines until the flush: the bytes after it are the pack.
  const commands: Array<{ oldOid: string; newOid: string; ref: string; caps: string }> = [];
  let offset = 0;
  let packStart = body.length;
  let sideband = false;
  while (offset + 4 <= body.length) {
    const header = body.subarray(offset, offset + 4).toString('ascii');
    if (!/^[0-9a-fA-F]{4}$/.test(header)) throw new Error('invalid pkt-line in receive-pack');
    const length = Number.parseInt(header, 16);
    if (length === 0) {
      offset += 4;
      packStart = offset;
      break;
    }
    if (length < 4 || offset + length > body.length) throw new Error('truncated pkt-line in receive-pack');
    const payload = body.subarray(offset + 4, offset + length).toString('utf8');
    const nul = payload.indexOf('\u0000');
    const line = (nul >= 0 ? payload.slice(0, nul) : payload).replace(/\n$/, '');
    const caps = nul >= 0 ? payload.slice(nul + 1).replace(/\n$/, '') : '';
    const parts = line.split(' ');
    if (parts.length !== 3) throw new Error('bad receive-pack command line');
    commands.push({ oldOid: parts[0] as string, newOid: parts[1] as string, ref: parts[2] as string, caps });
    if (caps.includes('side-band-64k')) sideband = true;
    offset += length;
  }
  if (commands.length === 0) throw new Error('receive-pack with no commands');

  // Unpack first: a corrupt pack fails the unpack, never the refs.
  let unpackError: string | null = null;
  const packBytes = body.subarray(packStart);
  if (packBytes.length > 0) {
    try {
      const objects = readPack(packBytes, (oid) => repo.objects.get(oid));
      for (const [oid, object] of objects) {
        if (!repo.objects.has(oid)) repo.objects.set(oid, object);
      }
    } catch (error) {
      unpackError = error instanceof Error ? error.message : String(error);
    }
  }

  const reportLines: string[] = [];
  if (unpackError !== null) {
    reportLines.push(`unpack ${unpackError}\n`);
  } else {
    reportLines.push('unpack ok\n');
    for (const command of commands) {
      const reason = validatePush(repo, command);
      if (reason === null) {
        if (command.newOid === ZERO_OID) repo.refs.delete(command.ref);
        else repo.refs.set(command.ref, command.newOid);
        reportLines.push(`ok ${command.ref}\n`);
      } else {
        reportLines.push(`ng ${command.ref} ${reason}\n`);
      }
    }
  }
  const reportStream = encodePktLines([...reportLines, 'flush' as const]);
  const parts: Buffer[] = [];
  if (sideband) {
    parts.push(encodePktLine(Buffer.concat([Buffer.from([1]), reportStream])));
  } else {
    parts.push(reportStream);
  }
  parts.push(PKT_FLUSH);
  sendGit(res, 200, Buffer.concat(parts), 'application/x-git-receive-pack-result');
}

/** Null when the push command may apply, else the ng reason. */
function validatePush(repo: Repo, command: { oldOid: string; newOid: string; ref: string }): string | null {
  const { oldOid, newOid, ref } = command;
  if (!isObjectId(oldOid) || !isObjectId(newOid)) return 'invalid object id';
  if (!ref.startsWith('refs/')) return 'invalid ref';
  const current = repo.refs.get(ref);
  if (newOid === ZERO_OID) {
    // Delete.
    if (!current) return 'remote ref does not exist';
    if (oldOid !== ZERO_OID && oldOid !== current) return 'non-fast-forward';
    return null;
  }
  const object = repo.objects.get(newOid);
  if (!object) return 'object not found';
  if (ref.startsWith('refs/heads/') && object.type !== 'commit') return 'not a commit';
  if (ref.startsWith('refs/tags/') && object.type !== 'commit' && object.type !== 'tag') return 'not a commit or tag';
  if (oldOid === ZERO_OID) {
    if (current) return 'remote ref already exists';
    return null;
  }
  if (!current) return 'remote ref does not exist';
  if (oldOid !== current) return 'non-fast-forward';
  if (newOid === current) return null;
  // Fast path: sequential pushes land on the current tip's child, so the new
  // commit's own parents name the current ref directly. Checking that is O(1);
  // the full history walk below is only for merges and longer jumps, and 200
  // sequential pushes would otherwise re-walk the whole history each time.
  const newObject = repo.objects.get(newOid);
  if (newObject !== undefined && newObject.type === 'commit') {
    if (parseCommit(newObject.data).parents.includes(current)) return null;
  }
  if (!isAncestor(current, newOid, (oid) => repo.objects.get(oid))) return 'non-fast-forward';
  return null;
}
