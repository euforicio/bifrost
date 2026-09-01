import assert from "node:assert/strict";
import { readFile } from "node:fs/promises";
import test from "node:test";

const matrix = JSON.parse(
  await readFile(new URL("../../provider-capabilities.json", import.meta.url), "utf8"),
).providers;

const expected = {
  cursor: { chat_completions: false, responses: true, responses_with_tools: true, list_models: true },
  "openai-codex": { chat_completions: false, responses: true, responses_with_tools: true, list_models: true },
  ollama: { chat_completions: true, text_completion: true, responses: true, embedding: true, list_models: true },
  fireworks: { chat_completions: true, text_completion: true, responses: true, embedding: true, list_models: true },
  xai: { chat_completions: true, text_completion: false, responses: true, responses_with_tools: true, image_generation: true, list_models: true },
};

test("production providers are represented by the capability matrix", () => {
  const canonicalKeys = Object.keys(matrix.openai).sort();
  for (const [provider, capabilities] of Object.entries(expected)) {
    assert.ok(matrix[provider], `missing provider capability row: ${provider}`);
    assert.deepEqual(Object.keys(matrix[provider]).sort(), canonicalKeys, `${provider} capability keys drifted`);
    for (const [operation, supported] of Object.entries(capabilities)) {
      assert.equal(matrix[provider][operation], supported, `${provider}.${operation}`);
    }
  }
});
