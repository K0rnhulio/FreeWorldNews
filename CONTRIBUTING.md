# Contributing to FreeWorldNews

Thanks for helping make the web easier to read.

For a site-specific issue, include a public article URL, what the reader shows,
and whether ordinary fetch, browser rendering, or HTML import was used. A short
redacted log is useful. Do not share cookies, account credentials, browser
profiles, or full copyrighted article fixtures.

Keep changes focused. For extraction fixes, add a small synthetic example that
captures the real page structure and verifies article selection, reading order,
and sanitization. Preserve the public-address network checks and upstream credits.

Before opening a pull request, run:

```sh
go test ./...
go vet ./...
npm ci --prefix browser
npm test --prefix browser
```

The optional [browser integration test](docs/development.md) is useful for worker
changes. A successful fetch of one publisher does not establish universal access;
describe the tested behavior and any remaining limits in your pull request.
