# Development and diagnostics

CI checks Go on Linux and Windows, and the optional worker on Node.js 24.

```sh
go test ./...
go vet ./...
npm ci --prefix browser
npm test --prefix browser
```

## Real browser integration

Install the worker and matching Chromium first. This opt-in test renders a
synthetic article, verifies sanitization, and checks that redirects and page
subresources cannot reach a private-address canary.

```sh
FREEWORLDNEWS_BROWSER_TEST=1 go test -run TestBrowserIntegration -v .
```

PowerShell:

```powershell
$env:FREEWORLDNEWS_BROWSER_TEST = '1'
go test -run TestBrowserIntegration -v .
```

## Check a live publisher

Set `FREEWORLDNEWS_BROWSER_TEST_URL` to a public article URL, then run
`go test -run TestBrowserLive -v .`. The test reports the extracted word count;
short excerpts are explicitly marked. Loading a page is not proof that the
full article was recovered.

| Optional variable | Effect |
| --- | --- |
| `FREEWORLDNEWS_BROWSER_TEST_CHANNEL=chrome` | Use installed Chrome |
| `FREEWORLDNEWS_BROWSER_TEST_HEADFUL=1` | Show the browser |
| `FREEWORLDNEWS_BROWSER_TEST_SCREENSHOT` | Save a screenshot to an absolute PNG path |
| `FREEWORLDNEWS_BROWSER_TEST_DIAGNOSTICS` | Save a bounded local diagnostic capture |

Diagnostic captures include rendered HTML, a request manifest, and same-host
HTML/JSON responses. They do not record request headers or cookies. Captures
can contain publisher page data; keep them local and out of commits.

Set `FREEWORLDNEWS_READER_SNAPSHOT` to a saved HTML file and
`FREEWORLDNEWS_BROWSER_TEST_URL` to its original URL, then run
`go test -run TestArticleSnapshot -v .` to inspect extraction without fetching
the publisher again. The older `FREENEWS_` test-variable prefix is also accepted.

## Extraction

The reader compares visible paragraph containers with embedded JSON article
bodies. Arc/Fusion `Fusion.globalContent` assignments are decoded as JSON
without evaluating JavaScript. Editorial block arrays preserve paragraph order,
headings, and quotations; ads and paywall widgets are omitted. Canonical URLs
help avoid selecting a different story from page data.

Every selected body goes through the same HTML allowlist. Add synthetic fixtures
when extending an extraction format. Never commit a publisher's full article,
authentication tokens, personal browser profile, or captured cookies.

## Network behavior

Direct fetches check the resolved destination address before connecting.
Browser traffic uses an authenticated loopback proxy backed by the same
public-address-only dialer. Browser redirects and subresources use that proxy;
ports are restricted to 80 and 443. Chromium's sandbox stays enabled.

Browser workers are separate Node processes. Cancellation first requests
graceful shutdown, with process-tree termination as a fallback. Browser profiles
are temporary and never reuse a personal Chrome profile.
