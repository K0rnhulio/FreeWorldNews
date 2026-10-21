import { test } from 'node:test';
import assert from 'node:assert/strict';
import { render, validateInput } from './render.mjs';

const base = { url: 'https://example.com/article', channel: 'chromium', proxy: { server: 'http://127.0.0.1:12345', password: 'test' } };

test('invalid destinations and missing egress proxy fail before launching a browser', async () => {
  for (const input of [
    { ...base, url: 'file:///etc/passwd' },
    { ...base, url: 'https://user:secret@example.com/' },
    { ...base, channel: 'firefox' },
    { ...base, proxy: { ...base.proxy, server: 'http://proxy.example.com:12345' } },
  ]) {
    await assert.rejects(render(input));
  }
  assert.equal(validateInput(base).hostname, 'example.com');
});
