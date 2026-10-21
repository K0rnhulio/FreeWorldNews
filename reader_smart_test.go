package main

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"html/template"
	"io"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"
)

type readerRoundTrip func(*http.Request) (*http.Response, error)

func (f readerRoundTrip) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

func mockReader(t *testing.T, f readerRoundTrip) {
	t.Helper()
	oldClient, oldHistory, oldConfig, oldRenderer := readerClient, readerHistory, config, readerRenderer
	readerRenderer = nil
	readerHistory = newReaderMemory()
	config = Config{}
	readerClient = &http.Client{Transport: f}
	t.Cleanup(func() {
		readerClient, readerHistory, config, readerRenderer = oldClient, oldHistory, oldConfig, oldRenderer
	})
}

func readerResponse(r *http.Request, status int, body string) *http.Response {
	return &http.Response{
		StatusCode: status, Status: fmt.Sprintf("%d %s", status, http.StatusText(status)),
		Header: http.Header{"Content-Type": {"text/html"}}, Body: io.NopCloser(strings.NewReader(body)), Request: r,
	}
}

func articleURL(path string) *url.URL {
	u, _ := url.Parse("https://news.example.com/" + path)
	return u
}

func TestReaderNormalFirstAndCache(t *testing.T) {
	calls := 0
	mockReader(t, func(r *http.Request) (*http.Response, error) {
		calls++
		if r.Header.Get("User-Agent") != browserUA || r.Header.Get("X-Forwarded-For") != "" || r.Header.Get("Referer") != "" {
			t.Fatal("first request should use ordinary headers")
		}
		return readerResponse(r, 200, "<article><p>"+words(220)+"</p></article>"), nil
	})
	a, err := fetchArticle(context.Background(), articleURL("one#first"))
	if err != nil || a.Words != 220 {
		t.Fatalf("article=%+v err=%v", a, err)
	}
	a.Title = "changed by caller"
	b, err := fetchArticle(context.Background(), articleURL("one#second"))
	if err != nil || calls != 1 || b.Title == a.Title {
		t.Fatalf("cache should serve an independent copy: calls=%d article=%+v err=%v", calls, b, err)
	}
}

func TestReaderLearnsWorkingMode(t *testing.T) {
	var agents []string
	mockReader(t, func(r *http.Request) (*http.Response, error) {
		agents = append(agents, r.Header.Get("User-Agent"))
		n := 30
		if strings.HasPrefix(r.Header.Get("User-Agent"), "AdsBot") {
			n = 200
		}
		return readerResponse(r, 200, "<article><p>"+words(n)+"</p></article>"), nil
	})
	for _, path := range []string{"one", "two"} {
		if _, err := fetchArticle(context.Background(), articleURL(path)); err != nil {
			t.Fatal(err)
		}
	}
	if len(agents) != 3 || agents[0] != browserUA || !strings.HasPrefix(agents[1], "AdsBot") || agents[2] != agents[1] {
		t.Fatalf("unexpected strategy order: %v", agents)
	}
	if got := readerHistory.modes("news.example.com", true)[0]; got != modeArchive {
		t.Fatal("explicit archive preference should override learned mode")
	}
}

func TestReaderDataDomeSkipsDuplicateAttempts(t *testing.T) {
	var hosts []string
	mockReader(t, func(r *http.Request) (*http.Response, error) {
		hosts = append(hosts, r.URL.Hostname())
		if r.URL.Hostname() == archiveHost {
			return readerResponse(r, 404, "No snapshot"), nil
		}
		res := readerResponse(r, 403, `<p id="cmsg">Please enable JS and disable any ad blocker</p><script src="https://ct.captcha-delivery.com/c.js"></script>`)
		res.Header.Set("X-DataDome", "protected")
		return res, nil
	})
	for _, path := range []string{"one", "two"} {
		_, err := fetchArticle(context.Background(), articleURL(path))
		var wall *botWallError
		if !errors.As(err, &wall) || wall.Vendor != "DataDome" || !wall.NoArchive {
			t.Fatalf("expected DataDome and missing snapshot, got %v", err)
		}
	}
	if strings.Join(hosts, ",") != "news.example.com,web.archive.org,web.archive.org" {
		t.Fatalf("should avoid repeating challenged origin: %v", hosts)
	}
}

func TestReaderArchiveOutageIsNotMissingSnapshot(t *testing.T) {
	mockReader(t, func(r *http.Request) (*http.Response, error) {
		if r.URL.Hostname() == archiveHost {
			return readerResponse(r, 503, "Temporarily unavailable"), nil
		}
		res := readerResponse(r, 403, "Blocked")
		res.Header.Set("X-DataDome", "protected")
		return res, nil
	})
	_, err := fetchArticle(context.Background(), articleURL("one"))
	var wall *botWallError
	if !errors.As(err, &wall) || wall.NoArchive || strings.Contains(err.Error(), "has no copy") {
		t.Fatalf("an archive outage says nothing about whether a snapshot exists: %v", err)
	}
}

func TestReaderRateLimitFallsBackAndWaits(t *testing.T) {
	direct, archive := 0, 0
	mockReader(t, func(r *http.Request) (*http.Response, error) {
		if r.URL.Hostname() == archiveHost {
			archive++
			return readerResponse(r, 200, "<article><p>"+words(200)+"</p></article>"), nil
		}
		direct++
		res := readerResponse(r, 429, "Slow down")
		res.Header.Set("Retry-After", "120")
		return res, nil
	})
	for _, path := range []string{"one", "two"} {
		a, err := fetchArticle(context.Background(), articleURL(path))
		if err != nil || a.Source != "the Wayback Machine" {
			t.Fatalf("article=%+v err=%v", a, err)
		}
	}
	if direct != 1 || archive != 2 {
		t.Fatalf("direct=%d archive=%d", direct, archive)
	}
}

func TestReaderCancellationAndPrivateAddress(t *testing.T) {
	calls := 0
	ctx, cancel := context.WithCancel(context.Background())
	mockReader(t, func(r *http.Request) (*http.Response, error) {
		calls++
		cancel()
		return nil, context.Canceled
	})
	if _, err := fetchArticle(ctx, articleURL("one")); !errors.Is(err, context.Canceled) || calls != 1 {
		t.Fatalf("cancellation must stop fallbacks: calls=%d err=%v", calls, err)
	}
	u, _ := url.Parse("http://127.0.0.1/secrets")
	if _, err := fetchArticle(context.Background(), u); !errors.Is(err, errPrivateAddress) || calls != 1 {
		t.Fatalf("private address should never be requested: calls=%d err=%v", calls, err)
	}
}

func TestReaderChallengeDetection(t *testing.T) {
	cases := []struct {
		name, body, header string
		status             int
		blocked            bool
	}{
		{"large challenge", `<script>window._cf_chl_opt={}</script>` + strings.Repeat(" ", 80<<10), "", 200, true},
		{"ordinary vendor mention", `<article><p>We report on DataDome and PerimeterX today.</p></article>`, "", 200, false},
		{"article with comment captcha", `<article><p>` + words(200) + `</p></article><script src="https://challenges.cloudflare.com/turnstile/v0/api.js"></script>`, "", 200, false},
		{"explicit challenge header", `<p>` + words(200) + `</p>`, "challenge", 200, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			mockReader(t, func(r *http.Request) (*http.Response, error) {
				res := readerResponse(r, tc.status, tc.body)
				res.Header.Set("Cf-Mitigated", tc.header)
				return res, nil
			})
			_, err := fetchAndExtract(context.Background(), articleURL("one"), &HostOptions{}, modeBrowser)
			var wall *botWallError
			if errors.As(err, &wall) != tc.blocked {
				t.Fatalf("blocked=%v err=%v", tc.blocked, err)
			}
		})
	}
}

func TestReaderRejectsEmptyAndOversizePages(t *testing.T) {
	for _, body := range []string{"<html><body></body></html>", strings.Repeat("x", maxPageBytes+1)} {
		mockReader(t, func(r *http.Request) (*http.Response, error) { return readerResponse(r, 200, body), nil })
		if _, err := fetchAndExtract(context.Background(), articleURL("one"), &HostOptions{}, modeBrowser); err == nil {
			t.Fatal("empty and truncated responses must not be articles")
		}
	}
}

func TestReaderMemoryExpiryAndBounds(t *testing.T) {
	m := newReaderMemory()
	now := time.Now()
	m.now = func() time.Time { return now }
	a := &article{Words: 200, Content: "content", cacheable: true, cacheTTL: time.Minute}
	m.cache("one", a)
	m.rememberMode("news.example.com", modeTricks)
	m.pause("news.example.com", now.Add(time.Minute), errors.New("pause"))
	now = now.Add(31 * time.Minute)
	if m.cached("one") != nil || m.cooldown("news.example.com") != nil || m.modes("news.example.com", false)[0] != modeBrowser {
		t.Fatal("cache, cooldown and learned strategy must expire")
	}
	for i := 0; i < 300; i++ {
		key := fmt.Sprint(i)
		m.cache(key, a)
		m.rememberMode(key, modeTricks)
		m.pause(key, now.Add(time.Minute), errors.New("pause"))
	}
	if len(m.articles) > 64 || m.bytes > readerCacheBytes || len(m.preferred) > readerStateLimit || len(m.paused) > readerStateLimit {
		t.Fatal("reader state must stay bounded")
	}
	a.Content = template.HTML(strings.Repeat("x", readerCacheBytes+1))
	m.cache("too-large", a)
	if m.cached("too-large") != nil {
		t.Fatal("large articles must not exceed the cache budget")
	}
}

func TestReaderMemoryConcurrentAccess(t *testing.T) {
	m := newReaderMemory()
	var wg sync.WaitGroup
	for i := 0; i < 20; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			m.cache("one", &article{Words: 200, Content: "text", cacheable: true, cacheTTL: time.Minute})
			m.cached("one")
			m.rememberMode("example.com", modeBrowser)
			m.modes("example.com", false)
			m.pause("example.com", time.Now().Add(time.Minute), errors.New("wait"))
			m.cooldown("example.com")
		}()
	}
	wg.Wait()
}

func TestReaderCacheDirectivesAndRetryAfter(t *testing.T) {
	now := time.Now().UTC().Truncate(time.Second)
	for _, value := range []string{"no-store", "private", "no-cache", "public, max-age=0"} {
		if ttl := articleCacheTTL(http.Header{"Cache-Control": {value}}, now); ttl != 0 {
			t.Errorf("%s must not be cached, ttl=%s", value, ttl)
		}
	}
	if ttl := articleCacheTTL(http.Header{"Cache-Control": {"public, max-age=60"}, "Age": {"20"}}, now); ttl != 40*time.Second {
		t.Fatalf("cache age must count towards TTL: %s", ttl)
	}
	if ttl := articleCacheTTL(http.Header{"Cache-Control": {"public", "private"}}, now); ttl != 0 {
		t.Fatal("all Cache-Control headers must be checked")
	}
	for _, value := range []string{"120", now.Add(2 * time.Minute).Format(http.TimeFormat)} {
		if got := retryAfter(value, now); !got.Equal(now.Add(2 * time.Minute)) {
			t.Errorf("retryAfter(%q)=%s", value, got)
		}
	}
	if got := retryAfter("bad", now); !got.Equal(now.Add(time.Minute)) {
		t.Errorf("invalid Retry-After should have a safe default: %s", got)
	}
}

func TestReaderCooldownPage(t *testing.T) {
	mockReader(t, func(r *http.Request) (*http.Response, error) {
		res := readerResponse(r, http.StatusTooManyRequests, "Slow down")
		res.Header.Set("Retry-After", "120")
		return res, nil
	})
	r := httptest.NewRequest("GET", "/read?url=https://news.example.com/story", nil)
	w := httptest.NewRecorder()
	readHandler(w, r)
	if w.Code != http.StatusServiceUnavailable || w.Header().Get("Retry-After") == "" || !strings.Contains(w.Body.String(), "Open original article") {
		t.Fatalf("cooldown page status=%d headers=%v", w.Code, w.Header())
	}
}

func importRequest(t *testing.T, original, page string, file bool) *http.Request {
	t.Helper()
	var body bytes.Buffer
	w := multipart.NewWriter(&body)
	w.WriteField("url", original)
	if file {
		f, err := w.CreateFormFile("file", "article.html")
		if err != nil {
			t.Fatal(err)
		}
		io.WriteString(f, page)
	} else {
		w.WriteField("page", page)
	}
	w.Close()
	r := httptest.NewRequest(http.MethodPost, "http://reader.example/import", &body)
	r.Header.Set("Content-Type", w.FormDataContentType())
	return r
}

func TestBrowserImportSanitizesWithoutFetching(t *testing.T) {
	mockReader(t, func(r *http.Request) (*http.Response, error) {
		t.Fatal("import must not fetch any URL")
		return nil, nil
	})
	for _, file := range []bool{false, true} {
		r := importRequest(t, "https://www.kn-online.de/story", `<html><head><title>Saved story</title></head><body><article><p>`+words(200)+`</p><p><a href="/related">Read more</a><img src="/photo.jpg" onerror="alert(1)"></p><script>alert(1)</script><iframe src="http://127.0.0.1/"></iframe></article></body></html>`, file)
		w := httptest.NewRecorder()
		importHandler(w, r)
		body := w.Body.String()
		if w.Code != 200 || !strings.Contains(body, "your browser&#39;s saved page") || !strings.Contains(body, `href="https://www.kn-online.de/related"`) {
			t.Fatalf("status=%d body=%.500s", w.Code, body)
		}
		for _, bad := range []string{"<script", "onerror", "<iframe", "alert(1)"} {
			if strings.Contains(body, bad) {
				t.Errorf("imported content contains %q", bad)
			}
		}
		if w.Header().Get("Cache-Control") != "no-store" || !strings.Contains(w.Header().Get("Content-Security-Policy"), "default-src 'none'") {
			t.Fatal("imports need no-store and the reader CSP")
		}
		if len(readerHistory.articles) != 0 {
			t.Fatal("imported pages must never enter shared cache")
		}
	}
}

func TestBrowserImportErrors(t *testing.T) {
	cases := []struct {
		original, page string
		status         int
	}{
		{"javascript:alert(1)", "<p>text</p>", 400},
		{"https://user:secret@example.com/", "<p>text</p>", 400},
		{"https://example.com/", `<script src="https://ct.captcha-delivery.com/c.js"></script><p>Enable JavaScript</p>`, 422},
		{"https://example.com/", "<html></html>", 422},
		{"https://example.com/", strings.Repeat("x", maxPageBytes+1), 413},
	}
	for _, tc := range cases {
		w := httptest.NewRecorder()
		importHandler(w, importRequest(t, tc.original, tc.page, true))
		if w.Code != tc.status {
			t.Errorf("want status=%d got=%d", tc.status, w.Code)
		}
	}
	w := httptest.NewRecorder()
	importHandler(w, httptest.NewRequest(http.MethodGet, "/import", nil))
	if w.Code != 405 || w.Header().Get("Allow") != "POST" {
		t.Fatal("imports require POST")
	}
}
