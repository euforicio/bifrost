import test from 'node:test';
import assert from 'node:assert/strict';
import { mkdtemp, writeFile, symlink, rm } from 'node:fs/promises';
import { tmpdir } from 'node:os';
import { join } from 'node:path';
import { assetDigest, validatePin, validateReceipt } from './manual-release.mjs';

const receipt = { schema: 'bifrost-manual-release/v1', source_sha: 'a'.repeat(40), infrastructure_base: 'b'.repeat(40),
  image_id: 'sha256:' + 'c'.repeat(64), assets_sha256: 'd'.repeat(64) };
const publication = { source_sha: receipt.source_sha, image_id: receipt.image_id,
  image: 'registry.cloudflare.com/dd99a5f86972717d02d06b52cba6ab5d/orion-bifrost-native@sha256:' + 'e'.repeat(64) };
const pin = { source_sha: receipt.source_sha, images: { bifrost: publication.image } };
test('a release must bind source, exact image and production registry', () => {
  validateReceipt(receipt);
  validatePin(receipt, publication, pin);
  for (const bad of [
    { ...publication, source_sha: 'f'.repeat(40) },
    { ...publication, image_id: 'sha256:' + 'f'.repeat(64) },
    { ...publication, image: publication.image.replace('orion-bifrost-native', 'other-product') },
    { ...publication, image: publication.image.replace(/@sha256:.*/, ':latest') },
  ]) assert.throws(() => validatePin(receipt, bad, pin));
  assert.throws(() => validatePin(receipt, publication, { ...pin, source_sha: 'f'.repeat(40) }));
  assert.throws(() => validatePin(receipt, publication, { ...pin, images: { ...pin.images, rift: publication.image } }));
  assert.throws(() => validatePin(receipt, publication, { ...pin, images: { bifrost: publication.image.replace(/e{64}$/, 'f'.repeat(64)) } }));
});
test('frontend receipt detects modified or added assets and rejects external symlinks', async () => {
  const dir = await mkdtemp(join(tmpdir(), 'bifrost-release-test-'));
  try {
    await writeFile(join(dir, 'index.html'), '<html></html>');
    const original = await assetDigest(dir);
    assert.equal(original, await assetDigest(dir));
    await writeFile(join(dir, 'extra.js'), 'changed');
    assert.notEqual(original, await assetDigest(dir));
    await rm(join(dir, 'extra.js'));
    await writeFile(join(dir, 'index.html'), 'modified');
    assert.notEqual(original, await assetDigest(dir));
    await symlink('/etc/hosts', join(dir, 'external'));
    await assert.rejects(assetDigest(dir), /symlinks/);
  } finally { await rm(dir, { recursive: true, force: true }); }
});
