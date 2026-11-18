/**
 * Launcher for the github-core-clone-01-v4 forge.
 *
 * The entry is a static Go binary (`forge`, vendored source in this repo).
 * This file only owns the process lifecycle the make.fun environment
 * expects: start the binary, wait for its READY line, register the bound
 * port, answer the boot health checks, then long-poll the environment's
 * /done endpoint and exit 0 when the trial ends.
 *
 * No application logic lives here; every request is served by the Go
 * binary over plain HTTP on localhost.
 */
import { spawn, type ChildProcess } from 'node:child_process';
import { setTimeout as sleep } from 'node:timers/promises';

const ENV_URL = process.env.MAKEFUN_ENV_URL ?? '';
const BOOT_DEADLINE_MS = 20_000;
const HEALTH_DEADLINE_MS = 10_000;
const POLL_INTERVAL_MS = 250;
const MAX_POLL_MS = 15 * 60_000;

const startedAt = Date.now();
let child: ChildProcess | null = null;
let port = 0;

function fail(message: string, code = 1): never {
  process.stderr.write(`launcher: ${message}\n`);
  if (child) child.kill('SIGTERM');
  process.exit(code);
}

/** Read lines from a stream, calling `onLine` for each complete line. */
function watchLines(stream: NodeJS.ReadableStream, onLine: (line: string) => void): void {
  let pending = '';
  stream.on('data', (chunk: Buffer) => {
    pending += chunk.toString('utf8');
    let cut = pending.indexOf('\n');
    while (cut >= 0) {
      onLine(pending.slice(0, cut).replace(/\r$/, ''));
      pending = pending.slice(cut + 1);
      cut = pending.indexOf('\n');
    }
  });
}

async function waitForReady(): Promise<void> {
  return new Promise<void>((resolve, reject) => {
    const timer = setTimeout(() => reject(new Error('timed out waiting for READY')), BOOT_DEADLINE_MS);
    watchLines(child!.stdout!, (line) => {
      process.stdout.write(`${line}\n`);
      const match = /^READY port=(\d+)$/.exec(line);
      if (match) {
        clearTimeout(timer);
        port = Number(match[1]);
        resolve();
      }
    });
    watchLines(child!.stderr!, (line) => process.stderr.write(`${line}\n`));
    child!.on('exit', (code) => reject(new Error(`forge exited early with code ${code}`)));
  });
}

async function register(): Promise<void> {
  const response = await fetch(`${ENV_URL}/register`, {
    method: 'POST',
    headers: { 'content-type': 'application/json' },
    body: JSON.stringify({ port }),
  });
  if (!response.ok) throw new Error(`register failed: ${response.status}`);
}

async function healthChecks(): Promise<void> {
  const deadline = Date.now() + HEALTH_DEADLINE_MS;
  for (let i = 0; i < 2; i += 1) {
    let ok = false;
    while (Date.now() < deadline) {
      try {
        const response = await fetch(`http://127.0.0.1:${port}/healthz`);
        if (response.ok) {
          ok = true;
          break;
        }
      } catch {
        // The binary may still be warming up; retry until the deadline.
      }
      await sleep(50);
    }
    if (!ok) throw new Error('health check failed');
  }
}

async function pollDone(): Promise<void> {
  const deadline = Date.now() + MAX_POLL_MS;
  for (;;) {
    if (Date.now() > deadline) throw new Error('timed out waiting for /done');
    try {
      const response = await fetch(`${ENV_URL}/done`);
      if (response.ok) {
        const body = (await response.json()) as { done?: boolean };
        if (body.done) return;
      }
    } catch {
      // Transient env hiccup; keep polling.
    }
    await sleep(POLL_INTERVAL_MS);
  }
}

async function main(): Promise<void> {
  if (!ENV_URL) fail('MAKEFUN_ENV_URL is not set');
  child = spawn('./forge', [], {
    cwd: new URL('.', import.meta.url).pathname,
    stdio: ['ignore', 'pipe', 'pipe'],
    env: process.env,
  });
  child.on('error', (error) => fail(`failed to spawn forge: ${error.message}`));
  try {
    await waitForReady();
  } catch (error) {
    fail((error as Error).message);
  }
  if (port <= 0 || port > 65535) fail(`forge reported a bad port: ${port}`);
  try {
    await register();
    await healthChecks();
    await pollDone();
  } catch (error) {
    fail((error as Error).message);
  }
  child.kill('SIGTERM');
  process.stderr.write(`launcher: trial done after ${Date.now() - startedAt}ms\n`);
  process.exit(0);
}

void main();
