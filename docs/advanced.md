# Configuration and advanced modes

The reader works with any public article URL. Host rules in `config.toml` are
optional reader preferences and the allowlist for DNS/reverse-proxy mode.
`config.example.toml` is the clean starting configuration.

```toml
info_host = "freeworldnews.test"
upstream_dns = "1.1.1.1:53"

[[host]]
name = "example.com"
from_archive = false
disable_cookies = true
disable_js = true
```

The legacy `from_google_cache` setting remains an alias for `from_archive`.
There is no Google Cache fetch backend. Header options such as `googlebot_ua`,
`googlebot_ip`, and `social_referer` remain for compatibility; they do not turn
your server into a verified Google crawler.

## Reader flags

| Flag | Purpose |
| --- | --- |
| `-readerOnly` | Start only the reader; default port 8080 |
| `-readerPort 8088` | Select the reader port |
| `-readerBrowser browser/render.mjs` | Enable Patchright |
| `-readerBrowserChannel chrome` | Use installed Chrome; also accepts `chromium` or `msedge` |
| `-readerBrowserHeadful` | Show the browser window |
| `-readerNode /path/to/node` | Select a Node.js executable |

Each lookup has a 45-second overall limit, or 80 seconds with browser rendering.
Workers are limited to one at a time, with up to 28 seconds to render. Every
worker starts with a new temporary profile and removes it when finished.

Challenges pause the affected fetch engine for two minutes. HTTP 429/503
responses respect `Retry-After`, with a one-minute fallback for missing/invalid
values. Numeric delays are capped at 24 hours. Archive outages are distinguished
from a confirmed missing snapshot.

## Import from your browser

Drag **Read with FreeWorldNews** from the reader page to your bookmarks bar.
Open an article, complete any browser verification yourself, wait for it to
load, and click the bookmark. It submits the loaded HTML to your own reader;
it does not read the publisher's cookies.

If a site's security policy prevents submission, save the article as HTML and
use **Read a page saved from your browser** with the original URL. Imports are
limited to 10 MB, sanitized without executing scripts, and not shared in cache.

## DNS and reverse proxy

Omit `-readerOnly` to use the original DNS/proxy servers. Add `-readerPort 8080`
if you also want the reader. This mode creates or loads the CA in `cert/`.
Existing certificates are kept; newly generated certificates use FreeWorldNews
names. The configured `info_host` is the CA download and host management page.

1. Set the server's public address with `-publicIP YOUR_SERVER_IP` if automatic
   detection is not appropriate.
2. Add publisher hostnames to `config.toml`.
3. Configure your device to use this DNS server.
4. Open your configured info hostname and install the proxy CA on that device.

This proxy terminates HTTPS for configured hosts, so install its CA only on
devices where you intend to use that setup. Reader-only mode does not use a CA.

| Service | Default port |
| --- | --- |
| DNS | 53 UDP and TCP |
| HTTP proxy | 80 TCP |
| HTTPS proxy | 443 TCP |
| DNS over TLS, when enabled | 853 TCP |

For DNS over TLS, place your domain's certificate chain and key at
`cert/dot_cert.pem` and `cert/dot_key.pem`, then use `-dotDomain YOUR_DOMAIN`.
For Let's Encrypt, use `fullchain.pem` as the certificate chain.

Use `-mitmAAAA YOUR_IPV6_ADDRESS` to configure an IPv6 proxy address.
Add `-blockList domains.txt` to load a DNS domain blocklist.

## Run the reader as a Linux service

```ini
[Unit]
Description=FreeWorldNews Reader
After=network-online.target

[Service]
User=freeworldnews
WorkingDirectory=/opt/freeworldnews
ExecStart=/opt/freeworldnews/freeworldnews -readerOnly -readerPort 8080
Restart=on-failure

[Install]
WantedBy=multi-user.target
```

Create the service user and install the application/configuration in that
directory before enabling the service. Add browser flags to `ExecStart` only
after installing the worker dependencies for the service user.
