// Entry point: start the Go forge binary, learn its port from its stdout,
// register that port with the trial env, then wait for the env to finish and
// shut the forge down.
//
// The env lives at MAKEFUN_ENV_URL and owns port 8080, so the forge always
// binds an ephemeral port of its own (listen :0). Registration is POST
// /register {"port": N}; completion is GET /done until {"done": true}. Both
// are retried with exponential backoff because the env may still be starting
// when this process first talks to it.

import { spawn, type ChildProcess } from 'node:child_process';
import { copyFile, chmod, unlink } from 'node:fs/promises';
import { fileURLToPath } from 'node:url';
import os from 'node:os';
import path from 'node:path';

const envUrl = (process.env.MAKEFUN_ENV_URL ?? '').replace(/\/+$/, '');
if (envUrl === '') {
  process.stderr.write('[entry] MAKEFUN_ENV_URL is not set: there is no env to register with\n');
  process.exit(1);
}

const FETCH_TIMEOUT_MS = 30_000;
const LISTEN_TIMEOUT_MS = 30_000;
const BACKOFF_FIRST_MS = 250;
const BACKOFF_CAP_MS = 2_000;

const sleep = (ms: number): Promise<void> =>
  new Promise((resolve) => setTimeout(resolve, ms));

function backoffMs(attempt: number): number {
  return Math.min(BACKOFF_CAP_MS, BACKOFF_FIRST_MS * 2 ** Math.min(attempt, 8));
}

// The forge binary sits next to this file in the repository.
const forgePath = path.join(path.dirname(fileURLToPath(import.meta.url)), 'forge');

// Resolve a runnable copy of the forge. The trial pod mounts /bot read-only
// and a file uploaded through the GitHub web UI lands without its executable
// bit, so the committed binary may not be runnable where it sits. Copying it
// to the temp dir and marking it executable works in both cases; when the
// copy fails (or the original already runs), fall back to the original path.
async function resolveForgePath(): Promise<string> {
  const staged = path.join(os.tmpdir(), `forge-${process.pid}`);
  try {
    await copyFile(forgePath, staged);
    await chmod(staged, 0o755);
    return staged;
  } catch {
    return forgePath;
  }
}

function startForge(runnablePath: string): ChildProcess {
  const child = spawn(runnablePath, [], { stdio: ['ignore', 'pipe', 'pipe'] });
  child.stderr?.on('data', (chunk: Buffer) => process.stderr.write(chunk));
  return child;
}

// Read the child's stdout until it prints "listening on port N" and return N.
// Rejects when the child exits first or the line never arrives.
function waitForPort(child: ChildProcess): Promise<number> {
  return new Promise<number>((resolve, reject) => {
    let buffer = '';
    let settled = false;
    const timer = setTimeout(() => {
      if (settled) return;
      settled = true;
      child.kill('SIGTERM');
      reject(new Error('the forge did not print its port in time'));
    }, LISTEN_TIMEOUT_MS);
    const fail = (error: Error): void => {
      if (settled) return;
      settled = true;
      clearTimeout(timer);
      reject(error);
    };
    child.once('error', fail);
    child.once('exit', (code) => {
      fail(new Error(`the forge exited before listening (code ${code})`));
    });
    child.stdout?.on('data', (chunk: Buffer) => {
      buffer += chunk.toString('utf8');
      const match = /listening on port (\d+)/.exec(buffer);
      if (match === null) return;
      if (settled) return;
      settled = true;
      clearTimeout(timer);
      resolve(Number(match[1]));
    });
  });
}

// Register the bound port. 200 is the normal answer; 409 means another
// entry registered first, which this process accepts and waits on. 400/405
// are contract errors and end the program; anything else is retried.
async function registerPort(port: number): Promise<void> {
  for (let attempt = 0; ; attempt += 1) {
    try {
      const res = await fetch(`${envUrl}/register`, {
        method: 'POST',
        headers: { 'content-type': 'application/json' },
        body: JSON.stringify({ port }),
        signal: AbortSignal.timeout(FETCH_TIMEOUT_MS),
      });
      if (res.status === 200 || res.status === 409) return;
      if (res.status === 400 || res.status === 405) {
        throw new Error(`register answered HTTP ${res.status}: ${await res.text()}`);
      }
    } catch (err) {
      if (err instanceof Error && err.message.startsWith('register answered HTTP ')) {
        throw err;
      }
    }
    await sleep(backoffMs(attempt));
  }
}

// Poll GET /done until the env answers {"done": true}. The env answers
// immediately ({"done": false} while the suite runs), so this is a poll,
// not a long-held request. A failed or slow poll is retried; this function
// never returns before the env says the trial is done.
async function waitUntilDone(): Promise<void> {
  for (let attempt = 0; ; attempt += 1) {
    try {
      const res = await fetch(`${envUrl}/done`, { signal: AbortSignal.timeout(FETCH_TIMEOUT_MS) });
      if (res.ok) {
        const body = (await res.json()) as { done?: unknown };
        if (body.done === true) return;
      }
    } catch {
      // Not answering yet, or the poll timed out: not done, so poll again.
    }
    await sleep(backoffMs(attempt));
  }
}

async function main(): Promise<void> {
  const runnablePath = await resolveForgePath();
  const staged = runnablePath !== forgePath;
  const forge = startForge(runnablePath);
  const port = await waitForPort(forge);
  process.stdout.write(`[entry] listening on port ${port}\n`);

  await registerPort(port);
  process.stdout.write('[entry] registered; waiting for the env to say done\n');

  await waitUntilDone();
  forge.kill('SIGTERM');
  if (staged) {
    await unlink(runnablePath).catch(() => undefined);
  }
  process.stdout.write('[entry] done\n');
}

try {
  await main();
  process.exit(0);
} catch (err) {
  process.stderr.write(`[entry] ${err instanceof Error ? err.message : String(err)}\n`);
  process.exit(1);
}
