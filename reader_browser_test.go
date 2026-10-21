package main

import (
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func readerTestEnv(name string) string {
	if value, ok := os.LookupEnv("FREEWORLDNEWS_" + name); ok {
		return value
	}
	return os.Getenv("FREENEWS_" + name)
}

func TestRenderedFallbackAfterHTTPChallenge(t *testing.T) {
	var modes []string
	mockReader(t, func(r *http.Request) (*http.Response, error) {
		modes = append(modes, "http")
		res := readerResponse(r, 403, "Blocked")
		res.Header.Set("X-DataDome", "protected")
		return res, nil
	})
	readerRenderer = func(ctx context.Context, u *url.URL) (*article, error) {
		modes = append(modes, "rendered")
		return extractFetchedPage([]byte("<article><p>"+words(220)+"</p></article>"), u, u, 200, http.Header{}, modeRendered)
	}
	a, err := fetchArticle(context.Background(), articleURL("one"))
	if err != nil || a.Words != 220 || !strings.Contains(a.Source, "Patchright") || strings.Join(modes, ",") != "http,rendered" {
		t.Fatalf("modes=%v article=%+v err=%v", modes, a, err)
	}
	if len(readerHistory.articles) != 0 {
		t.Fatal("browser sessions must not populate shared cache")
	}
	if readerHistory.modes("news.example.com", false)[0] != modeRendered {
		t.Fatal("successful browser mode should be remembered")
	}
}

func TestRenderedFailureCooldownAndArchive(t *testing.T) {
	renders := 0
	mockReader(t, func(r *http.Request) (*http.Response, error) {
		if r.URL.Hostname() == archiveHost {
			return readerResponse(r, 200, "<article><p>"+words(20)+"</p></article>"), nil
		}
		res := readerResponse(r, 403, "Blocked")
		res.Header.Set("X-DataDome", "protected")
		return res, nil
	})
	readerRenderer = func(ctx context.Context, u *url.URL) (*article, error) {
		renders++
		return nil, errors.New("worker unavailable")
	}
	for _, path := range []string{"one", "two"} {
		a, err := fetchArticle(context.Background(), articleURL(path))
		if err != nil || a.Source != "the Wayback Machine" {
			t.Fatalf("article=%+v err=%v", a, err)
		}
	}
	if renders != 1 {
		t.Fatalf("failed browser launches must be cooled down, launches=%d", renders)
	}
}

func TestRateLimitDoesNotLaunchBrowser(t *testing.T) {
	mockReader(t, func(r *http.Request) (*http.Response, error) { return readerResponse(r, 429, "Slow down"), nil })
	readerRenderer = func(context.Context, *url.URL) (*article, error) {
		t.Fatal("browser must respect HTTP rate limits")
		return nil, nil
	}
	fetchArticle(context.Background(), articleURL("one"))
}

func TestRenderedBlockIsReported(t *testing.T) {
	mockReader(t, func(r *http.Request) (*http.Response, error) {
		if r.URL.Hostname() == archiveHost {
			return readerResponse(r, 404, "No copy"), nil
		}
		res := readerResponse(r, 503, "Challenge")
		res.Header.Set("Cf-Mitigated", "challenge")
		return res, nil
	})
	readerRenderer = func(ctx context.Context, u *url.URL) (*article, error) {
		return nil, &botWallError{Site: u.Hostname(), Vendor: "Cloudflare"}
	}
	_, err := fetchArticle(context.Background(), articleURL("one"))
	var wall *botWallError
	if !errors.As(err, &wall) || !wall.BrowserBlocked || !strings.Contains(err.Error(), "browser fallback also received") {
		t.Fatalf("browser challenge must be reported accurately: %v", err)
	}
}

func TestBrowserProxyDestinations(t *testing.T) {
	for _, target := range []string{"127.0.0.1:443", "[::1]:443", "169.254.169.254:80", "100.64.0.1:443", "example.com:22", "user@example.com:443", "example.com:0"} {
		if _, err := browserProxyAddress(target, true); err == nil {
			t.Errorf("must reject %s", target)
		}
	}
	if got, err := browserProxyAddress("example.com:443", true); err != nil || got != "example.com:443" {
		t.Fatalf("address=%s err=%v", got, err)
	}
}

func TestBrowserProxyAuthenticationAndHTTP(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	var dials int32
	origin := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Proxy-Authorization") != "" {
			t.Error("proxy credentials must not reach a publisher")
		}
		io.WriteString(w, "public article")
	}))
	defer origin.Close()
	p, err := newBrowserProxy(ctx, func(ctx context.Context, network, address string) (net.Conn, error) {
		atomic.AddInt32(&dials, 1)
		if address != "public.example:80" {
			return nil, errPrivateAddress
		}
		return (&net.Dialer{}).DialContext(ctx, network, strings.TrimPrefix(origin.URL, "http://"))
	})
	if err != nil {
		t.Fatal(err)
	}
	defer p.close()
	proxyURL, _ := url.Parse(p.Server)
	tr := &http.Transport{Proxy: http.ProxyURL(proxyURL)}
	defer tr.CloseIdleConnections()
	client := &http.Client{Transport: tr, Timeout: time.Second}
	res, err := client.Get("http://public.example/article")
	if err != nil {
		t.Fatal(err)
	}
	res.Body.Close()
	if res.StatusCode != 407 || atomic.LoadInt32(&dials) != 0 {
		t.Fatalf("unauthenticated request status=%d dials=%d", res.StatusCode, dials)
	}
	proxyURL.User = url.UserPassword(p.Username, p.Password)
	res, err = client.Get("http://public.example/article")
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(res.Body)
	res.Body.Close()
	if res.StatusCode != 200 || string(body) != "public article" {
		t.Fatalf("status=%d body=%s", res.StatusCode, body)
	}
	res, err = client.Get("http://127.0.0.1/secret")
	if err != nil {
		t.Fatal(err)
	}
	res.Body.Close()
	if res.StatusCode != 403 || atomic.LoadInt32(&dials) != 1 {
		t.Fatalf("private destination status=%d dials=%d", res.StatusCode, dials)
	}
}

func TestBrowserProxyConnectTunnel(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	origin := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { io.WriteString(w, "secure article") }))
	defer origin.Close()
	p, err := newBrowserProxy(ctx, func(ctx context.Context, network, address string) (net.Conn, error) {
		if address != "secure.example:443" {
			return nil, errPrivateAddress
		}
		return (&net.Dialer{}).DialContext(ctx, network, strings.TrimPrefix(origin.URL, "https://"))
	})
	if err != nil {
		t.Fatal(err)
	}
	defer p.close()
	proxyURL, _ := url.Parse(p.Server)
	proxyURL.User = url.UserPassword(p.Username, p.Password)
	// The test's temporary certificate is trusted only by this test client.
	tr := &http.Transport{Proxy: http.ProxyURL(proxyURL), TLSClientConfig: &tls.Config{InsecureSkipVerify: true}}
	defer tr.CloseIdleConnections()
	client := &http.Client{Transport: tr, Timeout: 2 * time.Second}
	res, err := client.Get("https://secure.example/story")
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(res.Body)
	res.Body.Close()
	if string(body) != "secure article" {
		t.Fatalf("tunnel returned %q", body)
	}
	if _, err := client.Get("https://127.0.0.1/secret"); err == nil {
		t.Fatal("private CONNECT destination must be rejected")
	}
}

// Opt in to this test after installing browser/package-lock.json dependencies
// and Chromium. It proves actual JavaScript rendering and protects a local
// canary from both subresource requests and redirects.
func TestBrowserIntegration(t *testing.T) {
	if readerTestEnv("BROWSER_TEST") != "1" {
		t.Skip("set FREEWORLDNEWS_BROWSER_TEST=1 to run Chromium integration")
	}
	var privateHits int32
	private := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&privateHits, 1)
		io.WriteString(w, "secret")
	}))
	defer private.Close()
	origin := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/redirect" {
			http.Redirect(w, r, private.URL+"/secret", 302)
			return
		}
		w.Header().Set("Content-Type", "text/html")
		fmt.Fprintf(w, `<html><head><title>Rendered fixture</title></head><body><article id="story"><p>Loading</p></article><script>fetch(%q).catch(()=>{});setTimeout(()=>{document.getElementById('story').innerHTML=%q},200)</script></body></html>`, private.URL+"/secret", "<p>"+words(220)+"</p><p><a href='/next' onclick='alert(1)'>Next story</a></p>")
	}))
	defer origin.Close()
	mockReader(t, func(r *http.Request) (*http.Response, error) {
		res := readerResponse(r, 403, "Blocked")
		res.Header.Set("X-DataDome", "protected")
		return res, nil
	})
	b, err := newCommandBrowser("node", "browser/render.mjs", "chromium", false)
	if err != nil {
		t.Fatal(err)
	}
	b.dial = func(ctx context.Context, network, address string) (net.Conn, error) {
		if address != "fixture.example:80" {
			return nil, errPrivateAddress
		}
		return (&net.Dialer{}).DialContext(ctx, network, strings.TrimPrefix(origin.URL, "http://"))
	}
	readerRenderer = b.fetch
	u, _ := url.Parse("http://fixture.example/article")
	a, err := fetchArticle(context.Background(), u)
	if err != nil || a.Words < 220 || a.Title != "Rendered fixture" {
		t.Fatalf("article=%+v err=%v", a, err)
	}
	if strings.Contains(string(a.Content), "onclick") || !strings.Contains(string(a.Content), `href="http://fixture.example/next"`) {
		t.Fatalf("rendered article must be sanitized: %s", a.Content)
	}
	u, _ = url.Parse("http://fixture.example/redirect")
	if _, err := b.fetch(context.Background(), u); err == nil {
		t.Fatal("private redirect must not yield an article")
	}
	if atomic.LoadInt32(&privateHits) != 0 {
		t.Fatal("browser reached a private address")
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := b.fetch(ctx, u); !errors.Is(err, context.Canceled) {
		t.Fatalf("cancelled render returned %v", err)
	}
}

func TestBrowserLive(t *testing.T) {
	raw := readerTestEnv("BROWSER_TEST_URL")
	if raw == "" {
		t.Skip("set FREEWORLDNEWS_BROWSER_TEST_URL for a live publisher test")
	}
	oldConfig, oldHistory := config, readerHistory
	config.UpstreamDNS = "1.1.1.1:53"
	readerHistory = newReaderMemory()
	t.Cleanup(func() { config, readerHistory = oldConfig, oldHistory })
	channel := readerTestEnv("BROWSER_TEST_CHANNEL")
	if channel == "" {
		channel = "chromium"
	}
	headful := readerTestEnv("BROWSER_TEST_HEADFUL") == "1"
	b, err := newCommandBrowser("node", "browser/render.mjs", channel, headful)
	if err != nil {
		t.Fatal(err)
	}
	b.screenshot = readerTestEnv("BROWSER_TEST_SCREENSHOT")
	b.diagnostics = readerTestEnv("BROWSER_TEST_DIAGNOSTICS")
	t.Logf("browser channel=%s visible=%v", channel, headful)
	u, err := url.Parse(raw)
	if err != nil {
		t.Fatal(err)
	}
	a, err := b.fetch(context.Background(), u)
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("title=%q words=%d source=%s", a.Title, a.Words, a.Source)
	if a.Words < minArticleLen {
		t.Log("EXCERPT ONLY: loading a publisher page does not establish full-article retrieval")
	}
}
