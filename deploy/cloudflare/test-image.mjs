// Run the exact shipped image against real workerd SQL/R2, then replace both runtimes.
import { Miniflare, convertV4MiniflareOptions } from 'miniflare';
import { mkdtemp, readFile, writeFile, rm } from 'node:fs/promises';
import { tmpdir } from 'node:os';
import { join } from 'node:path';
import { fileURLToPath } from 'node:url';
import { spawn, spawnSync } from 'node:child_process';
import { randomBytes } from 'node:crypto';
import assert from 'node:assert/strict';

const [image, sha] = process.argv.slice(2);
const registryImage = /^ghcr\.io\/euforicio\/bifrost@sha256:[a-f0-9]{64}$/.test(image || '');
const localImage = /^sha256:[a-f0-9]{64}$/.test(image || '');
if (process.platform !== 'linux' || (!registryImage && !localImage) || !/^[a-f0-9]{40}$/.test(sha || '')) throw new Error('Linux runner and checked immutable Bifrost image required');
const directory = await mkdtemp(join(tmpdir(), 'bifrost-image-'));
const root = fileURLToPath(new URL('./', import.meta.url));
const token = randomBytes(32).toString('hex');
const name = `bifrost-native-${randomBytes(8).toString('hex')}`;
const password = randomBytes(32).toString('hex');
const privateValues = [token, password];
function docker(args, required = true) {
  const result = spawnSync('docker', args, { encoding: 'utf8', timeout: 120000 });
  if (required && result.status !== 0) throw new Error('Image qualification Docker operation failed; private output withheld');
  return result.stdout;
}
async function stopContainer() {
  await new Promise((resolve, reject) => {
    const child = spawn('docker', ['stop', '--time', '30', name], { stdio: 'ignore', timeout: 40000 });
    child.on('error', reject);
    child.on('exit', code => code === 0 ? resolve() : reject(new Error('Native image graceful stop failed')));
  });
}
const modules = await Promise.all(['qualification.js', 'storage.js', 'r2.js'].map(async file => ({ type: 'ESModule', path: join(root, file), contents: await readFile(join(root, file), 'utf8') })));
const options = convertV4MiniflareOptions({ modulesRoot: root, modules, port: 18884, compatibilityDate: '2026-10-09', durableObjects: { FILES: { className: 'SQLiteFiles', useSQLite: true } }, r2Buckets: ['BIFROST_DATA'], bindings: { STORAGE_TOKEN: token, BIFROST_R2_CAPABILITY: token, BIFROST_R2_BUCKET: 'bifrost-data' }, resourcePersistencePath: join(directory, 'authority') });
let runtime;
const sizes = {};
try {
  if (registryImage) docker(['pull', '--platform', 'linux/amd64', image]);
  const labels = JSON.parse(docker(['image', 'inspect', image, '--format', '{{json .Config.Labels}}']));
  assert.equal(labels['org.opencontainers.image.revision'], sha);
  assert.equal(labels['org.opencontainers.image.source'], 'https://github.com/euforicio/bifrost');
  assert.equal(docker(['image', 'inspect', image, '--format', '{{.Os}}/{{.Architecture}}']).trim(), 'linux/amd64');
  for (let cycle = 0; cycle < 2; cycle++) {
    runtime = new Miniflare(options);
    const origin = (await runtime.ready).toString().replace(/\/$/, '');
    if (cycle) {
      for (const store of ['config', 'logs']) {
        const response = await runtime.dispatchFetch(`${origin}/${store}/__readiness`, { headers: { 'X-Storage-Capability': token } });
        assert.equal(response.status, 200); const state = await response.json();
        assert.ok(state.ready); assert.equal(state.size, sizes[store]);
      }
    }
    const generation = randomBytes(32).toString('hex');
    privateValues.push(generation);
    for (const store of ['config', 'logs']) {
      const activated = await runtime.dispatchFetch(`${origin}/${store}/__activate`, { method: 'POST', headers: { 'X-Storage-Capability': token }, body: JSON.stringify({ capability: generation }) });
      assert.equal(activated.status, 200); await activated.arrayBuffer();
    }
    const env = {
      APP_HOST: '127.0.0.1', APP_PORT: '18885', APP_DIR: '/app/data', OTEL_SDK_DISABLED: 'true',
      BIFROST_ADMIN_USERNAME: 'native-qualification', BIFROST_ADMIN_PASSWORD: password,
      BIFROST_ENCRYPTION_KEY: token, BIFROST_SETUP_TOKEN: token, BIFROST_DEPLOYMENT_REVISION: sha,
      BIFROST_CLOUDFLARE_CONFIG_SQL_URL: `${origin}/config`, BIFROST_CLOUDFLARE_LOGS_SQL_URL: `${origin}/logs`,
      SQLITE_STORAGE_TOKEN: token, SQLITE_STORAGE_GENERATION_CAPABILITY: generation,
      BIFROST_CLOUDFLARE_R2_URL: `${origin}/objects`, BIFROST_CLOUDFLARE_R2_TOKEN: token
    };
    const file = join(directory, 'container.env');
    await writeFile(file, Object.entries(env).map(([key, value]) => `${key}=${value}`).join('\n') + '\n', { mode: 0o600 });
    docker(['run', '--detach', '--name', name, '--network', 'host', '--env-file', file, image]);
    let healthy = false;
    for (let attempt = 0; attempt < 120; attempt++) {
      try {
        const response = await fetch('http://127.0.0.1:18885/health', { signal: AbortSignal.timeout(2000) });
        const body = await response.json();
        healthy = response.ok && body.components?.db_pings === 'ok' && body.release_sha === sha && body.build_version === `v0.0.0-${sha}`;
        if (healthy) break;
      } catch {}
      await new Promise(resolve => setTimeout(resolve, 1000));
    }
    assert.ok(healthy, 'Exact shipped image must open native SQL/R2 and report its compiled revision');
    const login = await fetch('http://127.0.0.1:18885/api/session/login', { method: 'POST', headers: { 'Content-Type': 'application/json' }, body: JSON.stringify({ username: env.BIFROST_ADMIN_USERNAME, password }) });
    assert.equal(login.status, 200); assert.ok(login.headers.get('set-cookie')); await login.arrayBuffer();
    // Record the final durable size only after background writers have stopped.
    await stopContainer();
    for (const store of ['config', 'logs']) {
      const response = await runtime.dispatchFetch(`${origin}/${store}/__readiness`, { headers: { 'X-Storage-Capability': token } });
      assert.equal(response.status, 200); const state = await response.json();
      assert.ok(state.ready && state.size > 0);
      if (cycle) assert.ok(state.size >= sizes[store], 'Canonical native database retained across replacement');
      sizes[store] = state.size;
    }
    docker(['rm', '--force', name]);
    await runtime.dispose(); runtime = undefined;
  }
  console.log('Exact Cloudflare image passed native SQL/R2 startup, compiled revision, admin login and database retention across image/workerd replacement');
} catch (error) {
  const logs = spawnSync('docker', ['logs', '--tail', '200', name], { encoding: 'utf8', timeout: 10000 });
  let diagnostics = (logs.stdout || '') + (logs.stderr || '');
  for (const value of privateValues) diagnostics = diagnostics.replaceAll(value, '[REDACTED]');
  console.error('Native qualification startup diagnostics:\n' + diagnostics);
  console.error('Container exit code: ' + docker(['inspect', '--format', '{{.State.ExitCode}}', name], false).trim());
  throw error;
} finally {
  docker(['rm', '--force', name], false);
  if (runtime) await runtime.dispose();
  await rm(directory, { recursive: true, force: true });
}
