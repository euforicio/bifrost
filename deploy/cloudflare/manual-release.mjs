// Operator-owned release path. No GitHub Actions identity or receipt is fabricated.
import { spawnSync } from 'node:child_process';
import { createHash } from 'node:crypto';
import { cp, mkdir, mkdtemp, readFile, readdir, rm, writeFile } from 'node:fs/promises';
import { tmpdir } from 'node:os';
import { join, resolve } from 'node:path';
import { fileURLToPath } from 'node:url';

const root = fileURLToPath(new URL('../../', import.meta.url));
const account = 'dd99a5f86972717d02d06b52cba6ab5d';
const digest = value => createHash('sha256').update(value).digest('hex');
const shaPattern = /^[a-f0-9]{40}$/;
const imagePattern = new RegExp(`^registry\\.cloudflare\\.com/${account}/orion-bifrost-native@sha256:[a-f0-9]{64}$`);
function requireValue(ok, message) { if (!ok) throw new Error(message); }
function run(command, args, options = {}) {
  const { quiet = false, ...rest } = options;
  const result = spawnSync(command, args, { cwd: root, encoding: 'utf8', maxBuffer: 8 * 1024 * 1024,
    stdio: quiet ? ['pipe', 'pipe', 'pipe'] : 'inherit', ...rest });
  requireValue(result.status === 0, `${command} ${quiet ? '(private output withheld)' : args.join(' ')} failed`);
  return result.stdout?.trim();
}
const git = (cwd, ...args) => run('git', args, { cwd, quiet: true });
function clean(cwd) { requireValue(git(cwd, 'status', '--porcelain') === '', `Clean checkout required: ${cwd}`); }
function currentMain(cwd, repository) {
  clean(cwd);
  const sha = git(cwd, 'rev-parse', 'HEAD');
  const remote = run('gh', ['api', `repos/euforicio/${repository}/commits/main`, '--jq', '.sha'], { quiet: true });
  requireValue(sha === remote, `${repository} must be checked out at current remote main`);
  return sha;
}
async function json(path) { return JSON.parse(await readFile(path, 'utf8')); }
async function save(path, value) { await writeFile(path, JSON.stringify(value, null, 2) + '\n', { flag: 'wx', mode: 0o600 }); }

export async function assetDigest(directory) {
  const files = [];
  async function walk(path, relative = '') {
    for (const entry of (await readdir(path, { withFileTypes: true })).sort((a, b) => a.name.localeCompare(b.name, 'en'))) {
      const name = relative + entry.name;
      requireValue(!entry.isSymbolicLink(), 'Release assets must not contain symlinks');
      if (entry.isDirectory()) await walk(join(path, entry.name), name + '/');
      else { requireValue(entry.isFile(), 'Release assets must be regular files'); files.push([name, digest(await readFile(join(path, entry.name)))]); }
    }
  }
  await readFile(join(directory, 'index.html'));
  await walk(directory);
  return digest(JSON.stringify(files));
}
export function validateReceipt(receipt) {
  requireValue(receipt.schema === 'bifrost-manual-release/v1' && shaPattern.test(receipt.source_sha) &&
    shaPattern.test(receipt.infrastructure_base) && /^sha256:[a-f0-9]{64}$/.test(receipt.image_id) &&
    /^[a-f0-9]{64}$/.test(receipt.assets_sha256), 'Invalid manual qualification receipt');
}
export function validatePin(receipt, publication, pin) {
  validateReceipt(receipt);
  requireValue(publication.source_sha === receipt.source_sha && publication.image_id === receipt.image_id &&
    imagePattern.test(publication.image), 'Publication does not match the qualified image');
  requireValue(pin.source_sha === receipt.source_sha && Object.keys(pin.images || {}).join(',') === 'bifrost' &&
    pin.images.bifrost === publication.image, 'Land the exact published Bifrost release pin in infrastructure main before deployment');
}

async function main() {
  const [mode, infrastructurePath, outputPath] = process.argv.slice(2);
  if (mode === '--help' || !mode) {
    console.log('Usage: node deploy/cloudflare/manual-release.mjs <qualify|publish|deploy> <orion-infra checkout> <release directory>\n\nqualify: local tests, Linux image qualification, frontend and receipt\npublish: upload qualified image; emit infrastructure pin for review\ndeploy: deploy a matching pin from current infrastructure main, then frontend\n\nRequires Node 24.21.0, Go 1.27.4, Docker (Linux engine), gh and infrastructure npm dependencies.\nUses the existing Wrangler login or CLOUDFLARE_API_TOKEN. Never pass tokens as arguments.');
    return;
  }
  requireValue(['qualify', 'publish', 'deploy'].includes(mode) && infrastructurePath && outputPath && process.argv.length === 5,
    'Use --help for the three-stage release command');
  requireValue(process.versions.node === '24.21.0', 'Use production Node 24.21.0');
  const infra = resolve(infrastructurePath), output = resolve(outputPath);
  requireValue(!output.startsWith(resolve(root) + '/') && !output.startsWith(infra + '/'), 'Store release artifacts outside both source checkouts');
  const cf = join(infra, 'cloudflare'), wrangler = join(cf, 'node_modules/.bin/wrangler');
  const node = (args, options = {}) => run(process.execPath, args, options);
  const config = await json(join(cf, 'backends/native/wrangler.jsonc'));
  requireValue(config.account_id === account && config.name === 'rift-native-backends', 'Wrong production infrastructure');
  const env = { ...process.env, CLOUDFLARE_ACCOUNT_ID: account };
  const checkProduction = () => node([join(cf, 'scripts/require-production.mjs'), 'bifrost', root], { cwd: infra });

  if (mode === 'qualify') {
    clean(root);
    const sha = git(root, 'rev-parse', 'HEAD');
    const infraSha = currentMain(infra, 'orion-infra');
    requireValue(run('go', ['version'], { quiet: true }).includes('go1.27.4 '), 'Use patched Go 1.27.4');
    run('docker', ['info'], { quiet: true });
    checkProduction();
    await mkdir(output, { mode: 0o700 });
    run('bash', ['-c', 'source .github/workflows/scripts/setup-go-workspace.sh']);
    run('go', ['run', './.github/workflows/scripts/checkcloudflareworkflow.go', '.']);
    run('go', ['run', 'golang.org/x/vuln/cmd/govulncheck@v1.7.0', '-tags', 'sqlite_static,cloudflare', './bifrost-http'], { cwd: join(root, 'transports') });
    run('npm', ['audit', '--prefix', 'ui', '--package-lock-only', '--audit-level=low']);
    run('go', ['test', './providers/openaicodex', './providers/xai', './providers/cursor', './providers/fireworks', './providers/ollama', './providers/openai', './schemas'], { cwd: join(root, 'core') });
    run('go', ['test', './providercredentials', './configstore', './configstore/tables', './modelcatalog/datasheet'], { cwd: join(root, 'framework') });
    run('go', ['test', './bifrost-http/lib'], { cwd: join(root, 'transports') });
    run('go', ['test', './bifrost-http/handlers', '-run', 'ProviderCredential|ProviderAccount|Account|RemoveProvider'], { cwd: join(root, 'transports') });
    node(['--test', 'tests/e2e/api/runners/lib/provider-capabilities.test.mjs']);
    run('npm', ['ci', '--prefix', 'deploy/cloudflare']);
    node(['deploy/cloudflare/test.mjs']);
    run('npm', ['ci', '--prefix', 'ui']);
    run('npm', ['run', 'build-enterprise', '--prefix', 'ui']);
    await cp(join(root, 'ui/out'), join(output, 'web'), { recursive: true });
    run('docker', ['buildx', 'build', '--platform', 'linux/amd64', '--load', '--file', 'transports/Dockerfile.local',
      '--build-arg', `VERSION=0.0.0-${sha}`, '--build-arg', `VCS_REF=${sha}`, '--build-arg', 'CLOUDFLARE_STORAGE=true',
      '--iidfile', join(output, 'image-id'), '.']);
    const image = (await readFile(join(output, 'image-id'), 'utf8')).trim();
    run('docker', ['build', '--file', 'deploy/cloudflare/Dockerfile.qualify', '--iidfile', join(output, 'qualifier-id'), '.']);
    const qualifier = (await readFile(join(output, 'qualifier-id'), 'utf8')).trim();
    // The socket belongs to the selected Linux Docker engine, not the macOS host.
    run('docker', ['run', '--rm', '--network', 'host', '--mount', 'type=bind,source=/var/run/docker.sock,target=/var/run/docker.sock', qualifier, image, sha]);
    clean(root); clean(infra);
    requireValue(git(root, 'rev-parse', 'HEAD') === sha && currentMain(infra, 'orion-infra') === infraSha,
      'Source or infrastructure moved during qualification');
    await save(join(output, 'qualification.json'), { schema: 'bifrost-manual-release/v1', source_sha: sha,
      infrastructure_base: infraSha, image_id: image, assets_sha256: await assetDigest(join(output, 'web')), qualified_at: new Date().toISOString() });
    console.log(`Qualified ${sha}. Run publish with this same release directory.`);
    return;
  }

  const receipt = await json(join(output, 'qualification.json'));
  validateReceipt(receipt);
  requireValue(await assetDigest(join(output, 'web')) === receipt.assets_sha256, 'Qualified frontend assets changed');
  requireValue(currentMain(root, 'bifrost') === receipt.source_sha, 'Qualify the current Bifrost main revision before publishing or deploying');
  const infraSha = currentMain(infra, 'orion-infra');
  checkProduction();
  if (mode === 'publish') {
    requireValue(infraSha === receipt.infrastructure_base, 'Infrastructure changed since qualification; qualify again');
    const details = JSON.parse(run('docker', ['image', 'inspect', receipt.image_id], { quiet: true }))[0];
    requireValue(details.Id === receipt.image_id && details.Os === 'linux' && details.Architecture === 'amd64' &&
      details.Config.Labels['org.opencontainers.image.revision'] === receipt.source_sha &&
      details.Config.Labels['org.opencontainers.image.source'] === 'https://github.com/euforicio/bifrost' &&
      details.Config.Labels['io.rift.storage.cloudflare'] === 'true', 'Qualified image provenance changed');
    const auth = JSON.parse(run(wrangler, ['containers', 'registries', 'credentials', 'registry.cloudflare.com', '--push', '--pull',
      '--expiration-minutes', '15', '--json', '--config', 'backends/native/wrangler.jsonc'], { cwd: cf, env, quiet: true }));
    const dockerConfig = await mkdtemp(join(tmpdir(), 'bifrost-registry-'));
    const endpoint = process.env.DOCKER_HOST || run('docker', ['context', 'inspect', '--format', '{{.Endpoints.docker.Host}}'], { quiet: true });
    const dockerEnv = { ...process.env, DOCKER_CONFIG: dockerConfig, DOCKER_CONTEXT: '', DOCKER_HOST: endpoint };
    const docker = args => run('docker', args, { env: dockerEnv, quiet: true });
    const target = `registry.cloudflare.com/${account}/orion-bifrost-native:sha-${receipt.source_sha}`;
    try {
      run('docker', ['login', 'registry.cloudflare.com', '--username', auth.username, '--password-stdin'], { env: dockerEnv, input: auth.password, quiet: true });
      docker(['tag', receipt.image_id, target]);
      const pushed = docker(['push', '--platform', 'linux/amd64', target]);
      const hash = pushed.match(/digest: (sha256:[a-f0-9]{64})/)?.[1];
      requireValue(hash, 'Registry did not acknowledge an immutable digest');
      const image = target.split(':')[0] + '@' + hash;
      requireValue(JSON.parse(docker(['buildx', 'imagetools', 'inspect', image, '--format', '{{json .Manifest}}'])).digest === hash,
        'Registry digest verification failed');
      requireValue(currentMain(root, 'bifrost') === receipt.source_sha && currentMain(infra, 'orion-infra') === infraSha,
        'Main moved during publication; image is uploaded but not released');
      await save(join(output, 'publication.json'), { source_sha: receipt.source_sha, image_id: receipt.image_id, image });
      await save(join(output, 'bifrost.json'), { source_sha: receipt.source_sha, images: { bifrost: image } });
    } finally { await rm(dockerConfig, { recursive: true, force: true }); }
    console.log(`Published. Review and land ${join(output, 'bifrost.json')} as cloudflare/releases/bifrost.json in infrastructure main, then run deploy.`);
    return;
  }

  const publication = await json(join(output, 'publication.json'));
  validatePin(receipt, publication, await json(join(cf, 'releases/bifrost.json')));
  requireValue(git(infra, 'diff', '--name-only', receipt.infrastructure_base, infraSha) === 'cloudflare/releases/bifrost.json',
    'Infrastructure changed beyond the reviewed Bifrost pin; qualify again');
  env.GITHUB_SHA = infraSha; // Actual checked infrastructure commit; not an Actions run.
  // Reuse the local OAuth login for the existing read-only SSO preflight without printing it.
  if (!env.CLOUDFLARE_API_TOKEN) env.CLOUDFLARE_API_TOKEN = run(wrangler, ['auth', 'token'], { cwd: cf, quiet: true });
  node([join(cf, 'scripts/prepare-native.mjs')], { cwd: infra, env });
  node([join(cf, 'scripts/require-bifrost-sso.mjs')], { cwd: infra, env });
  run('npm', ['ci', '--prefix', join(cf, 'backends/native')]);
  node([join(cf, 'scripts/prepare-frontend.mjs'), 'bifrost', join(output, 'web')], { cwd: infra, env });
  run(wrangler, ['deploy', '--config', 'backends/native/.deployment.jsonc', '--dry-run'], { cwd: cf, env });
  run(wrangler, ['deploy', '--config', 'frontends/bifrost/wrangler.jsonc', '--dry-run'], { cwd: cf, env });
  requireValue(currentMain(root, 'bifrost') === receipt.source_sha && currentMain(infra, 'orion-infra') === infraSha, 'Main moved before deployment');
  run(wrangler, ['deploy', '--config', 'backends/native/.deployment.jsonc'], { cwd: cf, env });
  node([join(cf, 'scripts/verify-native-release.mjs')], { cwd: infra, env });
  node([join(cf, 'scripts/verify-product-release.mjs'), 'bifrost', receipt.source_sha], { cwd: infra, env });
  run(wrangler, ['deploy', '--config', 'frontends/bifrost/wrangler.jsonc'], { cwd: cf, env });
  const html = await readFile(join(output, 'web/index.html'), 'utf8');
  const asset = html.match(/<script\b[^>]*\bsrc="(\/assets\/[^"?]+\.js)"/)?.[1];
  requireValue(asset && !asset.includes('..'), 'Built frontend entry asset required');
  const response = await fetch('https://bifrost.riftlabs.app' + asset, { redirect: 'error', cache: 'no-store', signal: AbortSignal.timeout(30000) });
  requireValue(response.ok && digest(Buffer.from(await response.arrayBuffer())) === digest(await readFile(join(output, 'web', asset))),
    'Deployed frontend entry asset differs from the qualified build');
  await save(join(output, `deployment-${Date.now()}.json`), { source_sha: receipt.source_sha, infrastructure_sha: infraSha,
    image: publication.image, assets_sha256: receipt.assets_sha256, deployed_at: new Date().toISOString() });
  console.log('Deployed and verified https://bifrost.riftlabs.app');
}

if (process.argv[1] && resolve(process.argv[1]) === fileURLToPath(import.meta.url)) {
  main().catch(error => { console.error(error.message); process.exitCode = 1; });
}
