import { chromium } from 'patchright';
import { mkdtemp, mkdir, rm, writeFile } from 'node:fs/promises';
import { tmpdir } from 'node:os';
import { basename, dirname, join, resolve } from 'node:path';
import { createInterface } from 'node:readline';
import { pathToFileURL } from 'node:url';

const maxPageBytes = 10 << 20;

export function validateInput(input) {
  const url = new URL(input.url);
  if (!['http:', 'https:'].includes(url.protocol) || url.username || url.password) {
    throw new Error('Only public http or https article URLs are supported');
  }
  if (!['chromium', 'chrome', 'msedge'].includes(input.channel)) {
    throw new Error('Unsupported browser channel');
  }
  const proxy = new URL(input.proxy.server);
  if (proxy.protocol !== 'http:' || proxy.hostname !== '127.0.0.1' || !proxy.port || !input.proxy.password) {
    throw new Error('The reader must supply its authenticated loopback proxy');
  }
  return url;
}

// Run a genuine Chromium engine, with Patchright's own patches and the
// browser's default fingerprint. No user-agent or navigator overrides.
export async function render(input, signal) {
  validateInput(input);
  const timeout = Math.min(28000, Math.max(1000, input.timeoutMs || 28000));
  const deadline = Date.now() + timeout;
  const profile = await mkdtemp(join(tmpdir(), 'freeworldnews-browser-'));
  let context;
  let expired = false;
  const close = () => context?.close().catch(() => {});
  const timer = setTimeout(() => { expired = true; close(); }, timeout);
  signal?.addEventListener('abort', close, { once: true });
  try {
    context = await chromium.launchPersistentContext(profile, {
      channel: input.channel,
      headless: input.headless !== false,
      viewport: null,
      chromiumSandbox: true,
      acceptDownloads: false,
      proxy: input.proxy,
      timeout: Math.min(timeout, 12000),
      args: [
        '--proxy-bypass-list=<-loopback>',
        '--disable-quic',
        '--force-webrtc-ip-handling-policy=disable_non_proxied_udp',
      ],
    });
    if (expired || signal?.aborted) throw new Error('Browser rendering was cancelled');
    const page = context.pages()[0] || await context.newPage();
    // Reading an article never needs an unsolicited popup or a download.
    context.on('page', popup => { if (popup !== page) popup.close().catch(() => {}); });
    page.on('dialog', dialog => dialog.dismiss().catch(() => {}));
    let response;
    const requests = [];
    const captured = [];
    const pending = [];
    let capturedBytes = 0;
    const diagnosticHost = new URL(input.url).hostname;
    page.on('response', res => {
      if (res.request().isNavigationRequest() && res.frame() === page.mainFrame()) response = res;
      if (!input.diagnosticsDir || requests.length >= 200) return;
      const type = res.request().resourceType();
      const contentType = res.headers()['content-type'] || '';
      const url = new URL(res.url());
      requests.push({ url: res.url(), status: res.status(), type, contentType });
      // Save only this publisher's documents/JSON, never request headers,
      // cookies or third-party analytics payloads. This is operator opt-in.
      if (url.hostname !== diagnosticHost || pending.length >= 12) return;
      if (!(type === 'document' || (['fetch', 'xhr'].includes(type) && contentType.includes('json')))) return;
      if (Number(res.headers()['content-length']) > maxPageBytes) return;
      pending.push(res.body().then(body => {
        if (body.length > maxPageBytes || capturedBytes + body.length > (20 << 20)) return;
        capturedBytes += body.length;
        captured.push({ url: res.url(), status: res.status(), contentType, body });
      }).catch(() => {}));
    });
    try {
      await page.goto(input.url, { waitUntil: 'domcontentloaded', timeout: Math.min(18000, timeout) });
      // Wait for hydration or an automatic verification redirect, but never
      // click a CAPTCHA or turn a verification screen into an article.
      await page.waitForFunction(() => {
        const paragraphs = [...document.querySelectorAll('p')];
        return paragraphs.reduce((n, p) => n + p.innerText.trim().split(/\s+/).filter(Boolean).length, 0) >= 150;
      }, undefined, { timeout: Math.max(1, Math.min(10000, deadline - Date.now() - 1000)) });
    } catch (error) {
      // A loaded short article or challenge is still useful to the extractor.
      if (!response || expired || signal?.aborted) throw error;
    }
    const html = await page.content();
    if (Buffer.byteLength(html) > maxPageBytes) throw new Error('The rendered page exceeds 10 MB');
    // Optional operator-requested diagnostic; never enabled by article input.
    if (input.screenshotPath) await page.screenshot({ path: input.screenshotPath });
    if (input.diagnosticsDir) {
      const directory = resolve(input.diagnosticsDir);
      await mkdir(directory, { recursive: true });
      await writeFile(join(directory, 'rendered.html'), html, { mode: 0o600 });
      const timerWait = Math.max(1, Math.min(1500, deadline - Date.now() - 500));
      let captureTimer;
      await Promise.race([Promise.allSettled(pending), new Promise(accept => { captureTimer = setTimeout(accept, timerWait); })]);
      clearTimeout(captureTimer);
      const snapshots = [];
      for (let i = 0; i < captured.length; i++) {
        const { body, ...metadata } = captured[i];
        const file = `response-${i}.${metadata.contentType.includes('json') ? 'json' : 'html'}`;
        await writeFile(join(directory, file), body, { mode: 0o600 });
        snapshots.push({ ...metadata, file, bytes: body.length });
      }
      await writeFile(join(directory, 'network.json'), JSON.stringify({ requests, snapshots }, null, 2), { mode: 0o600 });
    }
    const allHeaders = response?.headers() || {};
    const headers = {};
    for (const key of ['content-type', 'cache-control', 'retry-after', 'cf-mitigated', 'x-datadome']) {
      if (allHeaders[key]) headers[key] = allHeaders[key];
    }
    return { url: page.url(), status: response?.status() || 502, headers, html };
  } finally {
    clearTimeout(timer);
    signal?.removeEventListener('abort', close);
    await close();
    // Only remove the fresh temporary directory created by this invocation.
    if (dirname(resolve(profile)) !== resolve(tmpdir()) || !basename(profile).startsWith('freeworldnews-browser-')) {
      throw new Error('Unexpected browser profile path');
    }
    await rm(profile, { recursive: true, force: true, maxRetries: 3, retryDelay: 100 });
  }
}

async function main() {
  const lines = createInterface({ input: process.stdin, crlfDelay: Infinity });
  const controller = new AbortController();
  const shutdown = () => controller.abort();
  process.once('SIGTERM', shutdown);
  process.once('SIGINT', shutdown);
  try {
    const line = await new Promise((accept, reject) => {
      lines.once('line', accept);
      lines.once('close', () => reject(new Error('Missing browser request')));
    });
    // Go keeps stdin open until completion; EOF signals cancellation.
    process.stdin.once('end', shutdown);
    const result = await render(JSON.parse(line), controller.signal);
    process.stdout.write(JSON.stringify(result) + '\n');
  } catch (error) {
    process.stderr.write(String(error?.stack || error) + '\n');
    process.stdout.write(JSON.stringify({ error: String(error?.message || error) }) + '\n');
  } finally {
    lines.close();
    process.stdin.destroy();
  }
}

if (process.argv[1] && import.meta.url === pathToFileURL(process.argv[1]).href) await main();
