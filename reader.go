package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"html/template"
	"io"
	"log"
	"net"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"syscall"
	"time"

	"golang.org/x/net/html"
	"golang.org/x/net/html/atom"
)

// Reader mode: paste an article URL, get the unpaywalled text on a clean page.
// Needs no CA or DNS changes on the device, unlike the reverse proxy.

const (
	maxPageBytes  = 10 << 20
	minArticleLen = 150 // words. Below this, try the wayback machine too
)

var readerClient *http.Client

var errPrivateAddress = errors.New("that address is on a private network, so it can't be read")

// Interesting keys in embedded JSON (__NEXT_DATA__, ld+json, ...)
var jsonBodyKeys = map[string]bool{
	"body": true, "articleBody": true, "content": true, "bodyHtml": true,
	"html": true, "text": true, "articleText": true, "contentHtml": true,
}

var junkRegexp = regexp.MustCompile(`(?i)paywall|subscri|advert|newsletter|related|share|social|promo|comment|signup|sign-up|recommend|byline|toolbar|breadcrumb`)

type article struct {
	URL       string
	Title     string
	Site      string
	Image     string
	Content   template.HTML
	Words     int
	Source    string
	cacheable bool
	cacheTTL  time.Duration
}

func serveReader(port int) {
	readerClient = &http.Client{
		Timeout:   30 * time.Second,
		Transport: newReaderTransport(),
		CheckRedirect: func(req *http.Request, via []*http.Request) error {
			if len(via) >= 10 {
				return errors.New("too many redirects")
			}
			if !validArticleURL(req.URL) {
				return errors.New("the site redirected to an invalid article URL")
			}
			return readerHistory.cooldown(req.URL.Hostname())
		},
	}

	mux := http.NewServeMux()
	mux.HandleFunc("/", readerHomeHandler)
	mux.HandleFunc("/read", readHandler)
	mux.HandleFunc("/import", importHandler)
	mux.HandleFunc("/favicon.svg", brandIconHandler)
	server := &http.Server{
		Addr:              fmt.Sprintf(":%d", port),
		Handler:           mux,
		ReadHeaderTimeout: 30 * time.Second,
	}
	log.Printf("[READER] Listening on http://%s:%d", *publicIP, port)
	if err := server.ListenAndServe(); err != nil {
		log.Fatalf("[ERR] Reader: %s . Terminating.", err)
	}
}

// newReaderTransport resolves via the upstream DNS (not our own, which would
// point unpaywalled hosts back at us) and refuses private addresses, so the
// reader can't be used to poke at the local network
func newReaderTransport() *http.Transport {
	dialer := &net.Dialer{
		Timeout: 10 * time.Second,
		Resolver: &net.Resolver{
			PreferGo: true,
			Dial: func(ctx context.Context, network, address string) (net.Conn, error) {
				d := net.Dialer{Timeout: 3 * time.Second}
				return d.DialContext(ctx, "udp", config.UpstreamDNS)
			},
		},
		Control: func(network, address string, _ syscall.RawConn) error {
			host, _, err := net.SplitHostPort(address)
			if err != nil {
				return err
			}
			if ip := net.ParseIP(host); ip == nil || !isPublicIP(ip) {
				return errPrivateAddress
			}
			return nil
		},
	}
	return &http.Transport{
		DialContext:           dialer.DialContext,
		ForceAttemptHTTP2:     true,
		TLSHandshakeTimeout:   10 * time.Second,
		ResponseHeaderTimeout: 15 * time.Second,
		IdleConnTimeout:       90 * time.Second,
		MaxIdleConnsPerHost:   4,
	}
}

func isPublicIP(ip net.IP) bool {
	return !(ip.IsPrivate() || ip.IsLoopback() || ip.IsLinkLocalUnicast() ||
		ip.IsLinkLocalMulticast() || ip.IsUnspecified() || ip.IsMulticast() ||
		ip.Equal(net.IPv4bcast) ||
		// Carrier grade NAT
		(ip.To4() != nil && ip.To4()[0] == 100 && ip.To4()[1]&0xc0 == 64))
}

func readerHomeHandler(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path != "/" {
		http.NotFound(w, r)
		return
	}
	renderReader(w, http.StatusOK, readerPage{Bookmarklet: bookmarklet(r)})
}

func readHandler(w http.ResponseWriter, r *http.Request) {
	raw := strings.TrimSpace(r.URL.Query().Get("url"))
	if raw == "" {
		http.Redirect(w, r, "/", http.StatusFound)
		return
	}
	// Pasted text from share sheets often has a title in front of the link
	if i := strings.Index(raw, "http"); i > 0 {
		raw = strings.Fields(raw[i:])[0]
	}
	if !strings.Contains(raw, "://") {
		raw = "https://" + raw
	}

	page := readerPage{Input: raw, Bookmarklet: bookmarklet(r)}
	u, err := url.Parse(raw)
	if err != nil || !validArticleURL(u) {
		page.Error = "That doesn't look like a link to an article."
		renderReader(w, http.StatusBadRequest, page)
		return
	}
	page.Original = u.String()

	a, err := fetchArticle(r.Context(), u)
	if err != nil {
		log.Println("[READER]", u, err)
		page.Error = "Couldn't load the article: " + err.Error() + "."
		var wall *botWallError
		if errors.As(err, &wall) {
			page.Error = "This site blocks automated readers: " + err.Error() + "."
		}
		status := http.StatusBadGateway
		var cooldown *upstreamCooldownError
		if errors.As(err, &cooldown) {
			status = http.StatusServiceUnavailable
			w.Header().Set("Retry-After", cooldown.Until.UTC().Format(http.TimeFormat))
		}
		renderReader(w, status, page)
		return
	}
	log.Printf("[READER] %s: %d words from %s", u, a.Words, a.Source)
	page.Article = a
	renderReader(w, http.StatusOK, page)
}

func bookmarklet(r *http.Request) template.URL {
	scheme := "http"
	if r.TLS != nil {
		scheme = "https"
	}
	action, _ := json.Marshal(scheme + "://" + r.Host + "/import")
	// The bookmark posts loaded HTML without reading the publisher's cookies.
	return template.URL(`javascript:(function(){var f=document.createElement('form');f.method='POST';f.action=` + string(action) + `;f.target='_blank';f.enctype='multipart/form-data';[['url',location.href],['page',document.documentElement.outerHTML]].forEach(function(p){var i=document.createElement('input');i.type='hidden';i.name=p[0];i.value=p[1];f.appendChild(i)});document.body.appendChild(f);f.submit();f.remove()})()`)
}

func fetchArticle(ctx context.Context, u *url.URL) (*article, error) {
	if !validArticleURL(u) {
		return nil, errors.New("invalid article URL")
	}
	options := getHostOptions(strings.ToLower(u.Hostname()))
	if options == nil {
		options = &HostOptions{}
	}

	if ip := net.ParseIP(u.Hostname()); ip != nil && !isPublicIP(ip) {
		return nil, errPrivateAddress
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	keyURL := *u
	keyURL.Fragment = ""
	key := keyURL.String()
	if a := readerHistory.cached(key); a != nil {
		return a, nil
	}
	timeout := 45 * time.Second
	if readerRenderer != nil {
		timeout = 80 * time.Second
	}
	parentCtx := ctx
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	modes := readerHistory.modes(u.Hostname(), enabled(options.FromArchive, false))

	var best *article
	var firstErr error
	var wall *botWallError
	noArchive := false
	browserBlocked := false
	directStopped := false
	throttled := false
	for _, mode := range modes {
		if directStopped && mode != modeArchive && mode != modeRendered {
			continue
		}
		if throttled && mode != modeArchive {
			continue
		}
		if err := ctx.Err(); err != nil {
			if best != nil && parentCtx.Err() == nil {
				break
			}
			return nil, err
		}
		host := strings.ToLower(u.Hostname())
		if mode == modeArchive {
			host = archiveHost
		}
		err := readerHistory.cooldown(host)
		if mode == modeRendered {
			// An HTTP challenge can still be rendered by a real browser.
			// Rate limits apply to both engines; browser challenges have
			// their own cooldown to prevent repeated Chromium launches.
			var challenge *botWallError
			if errors.As(err, &challenge) {
				err = nil
			}
			if err == nil {
				err = readerHistory.cooldown("browser:" + host)
			}
		}
		var a *article
		if err == nil {
			if mode == modeRendered {
				a, err = readerRenderer(ctx, u)
			} else {
				a, err = fetchAndExtract(ctx, u, options, mode)
			}
		}
		if ctx.Err() != nil {
			if best != nil && parentCtx.Err() == nil {
				break
			}
			return nil, ctx.Err()
		}
		if errors.Is(err, errPrivateAddress) {
			return nil, err
		}
		if err != nil {
			log.Printf("[READER] %s (%s): %s", u, mode, err)
			if firstErr == nil {
				firstErr = err
			}
			if mode == modeArchive {
				noArchive = errors.Is(err, errNoArchive)
			} else if wall == nil {
				errors.As(err, &wall)
			}
			var cooldown *upstreamCooldownError
			if mode != modeArchive && errors.As(err, &cooldown) {
				throttled = true
			}
			if mode != modeArchive && mode != modeRendered && (wall != nil || throttled) {
				directStopped = true
			}
			if mode == modeRendered {
				var browserWall *botWallError
				browserBlocked = errors.As(err, &browserWall)
				readerHistory.pause("browser:"+host, time.Now().Add(2*time.Minute), err)
			}
			continue
		}
		if best == nil || a.Words > best.Words {
			best = a
		}
		if best.Words >= minArticleLen {
			readerHistory.rememberMode(u.Hostname(), mode)
			break
		}
	}
	if best == nil {
		// The bot wall is the real reason, not e.g. a missing archive copy
		if wall != nil {
			wallCopy := *wall
			wallCopy.NoArchive = noArchive
			wallCopy.BrowserBlocked = browserBlocked
			return nil, &wallCopy
		}
		return nil, firstErr
	}
	readerHistory.cache(key, best)
	return best, nil
}

// Bot protection services answer with a challenge page instead of the
// article. We don't try to get past them, only explain what happened
var botWalls = []struct{ vendor, marker string }{
	{"DataDome", "captcha-delivery.com"},
	{"Cloudflare", "challenges.cloudflare.com"},
	{"Cloudflare", "cf_chl_opt"},
	{"Cloudflare", "cf-chl-"},
	{"Imperva", "_incapsula_resource"},
	{"HUMAN (PerimeterX)", "px-captcha"},
}

// detectBotWall returns the bot protection vendor if page is a challenge
// page. Inspect markup rather than article text or vendor names, with no
// page-size shortcut: real challenge pages can include large inline assets.
func detectBotWall(page []byte) string {
	doc, err := html.Parse(bytes.NewReader(page))
	if err != nil {
		return ""
	}
	vendor := ""
	walk(doc, func(n *html.Node) bool {
		if n.DataAtom == atom.Script || n.DataAtom == atom.Iframe || n.DataAtom == atom.Form || n.DataAtom == atom.Div {
			marker := strings.ToLower(attr(n, "src") + " " + attr(n, "action") + " " + attr(n, "id") + " " + attr(n, "class"))
			if n.DataAtom == atom.Script && n.FirstChild != nil {
				marker += " " + strings.ToLower(n.FirstChild.Data)
			}
			for _, w := range botWalls {
				if strings.Contains(marker, w.marker) {
					vendor = w.vendor
				}
			}
		}
		return n.DataAtom != atom.Script
	})
	return vendor
}

type botWallError struct {
	Site, Vendor   string
	NoArchive      bool
	BrowserBlocked bool
}

func (e *botWallError) Error() string {
	msg := fmt.Sprintf("%s returned a %s CAPTCHA or browser verification page", e.Site, e.Vendor)
	if e.BrowserBlocked {
		msg += ". The browser fallback also received a verification page"
	}
	if e.NoArchive {
		msg += ". The Wayback Machine has no copy"
	}
	msg += ". Open the original in your browser, then use Read with FreeWorldNews or import the saved HTML"
	return msg
}

type fetchMode string

const (
	modeTricks   fetchMode = "bot tricks"
	modeBrowser  fetchMode = "browser"
	modeArchive  fetchMode = "archive"
	modeRendered fetchMode = "Patchright"
)

const browserUA = "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/140.0 Safari/537.36"

func fetchAndExtract(ctx context.Context, u *url.URL, options *HostOptions, mode fetchMode) (*article, error) {
	timeout := 15 * time.Second
	if mode == modeArchive {
		timeout = 20 * time.Second
	}
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	target := u.String()
	if mode == modeArchive {
		target = fmt.Sprintf("https://%s/web/2id_/%s", archiveHost, u)
	}
	req, err := http.NewRequestWithContext(ctx, "GET", target, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Accept", "text/html,application/xhtml+xml")
	req.Header.Set("Accept-Language", "en-US,en;q=0.9")
	req.Header.Set("User-Agent", browserUA)
	if mode == modeTricks {
		// Same tricks as the reverse proxy
		if enabled(options.SocialReferer, true) {
			req.Header.Set("Referer", "https://t.co/")
		}
		if enabled(options.GooglebotUA, true) {
			req.Header.Set("User-Agent", "AdsBot-Google (+http://www.google.com/adsbot.html)")
		}
		if enabled(options.GooglebotIP, true) {
			req.Header.Set("X-Forwarded-For", "66.102.0.0")
		}
	}

	res, err := readerClient.Do(req)
	if err != nil {
		if errors.Is(err, errPrivateAddress) {
			return nil, errPrivateAddress
		}
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		return nil, errors.New("the site didn't respond or refused the connection")
	}
	defer res.Body.Close()
	if err := responseCooldown(u, res.Request.URL, res.StatusCode, res.Header, mode); err != nil {
		return nil, err
	}
	b, err := io.ReadAll(io.LimitReader(res.Body, maxPageBytes+1))
	if err != nil {
		return nil, err
	}
	return extractFetchedPage(b, u, res.Request.URL, res.StatusCode, res.Header, mode)
}

func responseCooldown(original, final *url.URL, status int, headers http.Header, mode fetchMode) error {
	if status != http.StatusTooManyRequests && status != http.StatusServiceUnavailable {
		return nil
	}
	// Cloudflare can use 503 for a browser challenge. If no retry delay was
	// requested, let the challenge detector permit one rendering attempt.
	if status == http.StatusServiceUnavailable && headers.Get("Cf-Mitigated") == "challenge" && headers.Get("Retry-After") == "" {
		return nil
	}
	host := original.Hostname()
	if mode == modeArchive {
		host = archiveHost
	}
	until := retryAfter(headers.Get("Retry-After"), time.Now())
	err := &upstreamCooldownError{Host: final.Hostname(), Until: until}
	readerHistory.pause(host, until, err)
	readerHistory.pause(final.Hostname(), until, err)
	return err
}

func extractFetchedPage(b []byte, u, final *url.URL, status int, headers http.Header, mode fetchMode) (*article, error) {
	if err := responseCooldown(u, final, status, headers, mode); err != nil {
		return nil, err
	}
	if len(b) > maxPageBytes {
		return nil, errors.New("the page is too large to read")
	}
	base := final
	if mode == modeArchive {
		base = u
	}
	a := extractArticle(b, base)
	// An article may embed Turnstile for its comment form. Never discard a
	// substantial article just because a protection script is also present.
	vendor := detectBotWall(b)
	explicitChallenge := false
	if headers.Get("Cf-Mitigated") == "challenge" {
		vendor = "Cloudflare"
		explicitChallenge = true
	} else if status == http.StatusForbidden && headers.Get("X-DataDome") != "" {
		vendor = "DataDome"
	}
	if vendor != "" && (explicitChallenge || status != http.StatusOK || a.Words < minArticleLen) {
		err := &botWallError{Site: strings.TrimPrefix(final.Hostname(), "www."), Vendor: vendor}
		until := time.Now().Add(2 * time.Minute)
		host, finalHost := u.Hostname(), final.Hostname()
		if mode == modeArchive {
			host = archiveHost
		}
		if mode == modeRendered {
			host, finalHost = "browser:"+host, "browser:"+finalHost
		}
		readerHistory.pause(host, until, err)
		readerHistory.pause(finalHost, until, err)
		return nil, err
	}
	if status != http.StatusOK {
		if mode == modeArchive && status == http.StatusNotFound {
			return nil, errNoArchive
		}
		return nil, fmt.Errorf("%s answered %d %s", final.Host, status, http.StatusText(status))
	}
	if ct := headers.Get("Content-Type"); ct != "" && !strings.Contains(ct, "html") {
		return nil, fmt.Errorf("not a web page (%s)", ct)
	}

	if a.Words == 0 {
		return nil, errors.New("no readable article text was found")
	}
	a.URL = u.String()
	a.cacheTTL = articleCacheTTL(headers, time.Now())
	a.cacheable = a.cacheTTL > 0
	a.Source = "the page"
	if mode == modeArchive {
		a.Source = "the Wayback Machine"
	}
	if mode == modeRendered {
		a.Source = "the rendered page (Patchright)"
		// Browser-created sessions must not enter the shared public cache.
		a.cacheable = false
	}
	return a, nil
}

// extractArticle picks the longest of: article text hidden in embedded JSON,
// or the page element with the most paragraph text
func extractArticle(page []byte, base *url.URL) *article {
	a := &article{URL: base.String(), Site: strings.TrimPrefix(base.Hostname(), "www.")}
	doc, err := html.Parse(bytes.NewReader(page))
	if err != nil {
		return a
	}

	var scripts []articleDataScript
	var ogTitle, title, h1 string
	var best *html.Node
	scores := map[*html.Node]float64{}
	walk(doc, func(n *html.Node) bool {
		switch n.DataAtom {
		case atom.Meta:
			switch attr(n, "property") {
			case "og:title":
				ogTitle = attr(n, "content")
			case "og:image":
				a.Image = resolveURL(base, attr(n, "content"))
			case "og:site_name":
				a.Site = attr(n, "content")
			}
		case atom.Title:
			if title == "" {
				title = textOf(n)
			}
		case atom.H1:
			if h1 == "" {
				h1 = textOf(n)
			}
		case atom.Script:
			if n.FirstChild != nil {
				script := n.FirstChild.Data
				if strings.Contains(strings.ToLower(attr(n, "type")), "json") {
					scripts = append(scripts, articleDataScript{data: script})
				} else {
					for _, data := range fusionArticleData(script, base) {
						scripts = append(scripts, articleDataScript{data: data, fusion: true})
					}
				}
			}
			return false
		case atom.P:
			// Credit paragraph text to its container, and less to the one above
			words := float64(countWords(textOf(n)))
			if words < 5 {
				break
			}
			if p := n.Parent; p != nil {
				scores[p] += words
				if p.Parent != nil {
					scores[p.Parent] += words / 2
				}
			}
		}
		return true
	})
	for n, s := range scores {
		if best == nil || s > scores[best] {
			best = n
		}
	}

	a.Title = firstNonEmpty(ogTitle, h1, title, a.Site)

	if best != nil {
		var buf bytes.Buffer
		sanitizeChildren(&buf, best, base)
		a.Content = template.HTML(buf.String())
		a.Words = countWords(htmlText(buf.String()))
	}

	for _, s := range scripts {
		body := ""
		if !s.fusion {
			body = longestJSONBody(s.data)
		}
		if structured := structuredArticleBody(s.data, base); countWords(htmlText(structured)) > countWords(htmlText(body)) {
			body = structured
		}
		if body == "" {
			continue
		}
		var buf bytes.Buffer
		if strings.Contains(body, "<") {
			if frag, err := html.Parse(strings.NewReader(body)); err == nil {
				sanitizeChildren(&buf, frag, base)
			}
		} else {
			for _, para := range strings.Split(body, "\n") {
				if para = strings.TrimSpace(para); para != "" {
					fmt.Fprintf(&buf, "<p>%s</p>\n", html.EscapeString(para))
				}
			}
		}
		if words := countWords(htmlText(buf.String())); words > a.Words {
			a.Content = template.HTML(buf.String())
			a.Words = words
		}
	}
	return a
}

// longestJSONBody finds the longest article-like string in a JSON document
func longestJSONBody(s string) string {
	var v interface{}
	if json.Unmarshal([]byte(s), &v) != nil {
		return ""
	}
	best, bestWords := "", 0
	var visit func(key string, v interface{})
	visit = func(key string, v interface{}) {
		switch v := v.(type) {
		case map[string]interface{}:
			for k, c := range v {
				visit(k, c)
			}
		case []interface{}:
			for _, c := range v {
				visit(key, c)
			}
		case string:
			if jsonBodyKeys[key] {
				if w := countWords(htmlText(v)); w > bestWords {
					best, bestWords = v, w
				}
			}
		}
	}
	visit("", v)
	return best
}

// Tags kept as is. Everything else not in dropTags is unwrapped
var keepTags = map[atom.Atom]bool{
	atom.P: true, atom.H2: true, atom.H3: true, atom.H4: true, atom.Blockquote: true,
	atom.Ul: true, atom.Ol: true, atom.Li: true, atom.Strong: true, atom.B: true,
	atom.Em: true, atom.I: true, atom.Br: true, atom.Figure: true, atom.Figcaption: true,
	atom.A: true, atom.Img: true, atom.Pre: true, atom.Code: true, atom.Hr: true,
	atom.Table: true, atom.Thead: true, atom.Tbody: true, atom.Tr: true, atom.Td: true, atom.Th: true,
}

var dropTags = map[atom.Atom]bool{
	atom.Script: true, atom.Style: true, atom.Noscript: true, atom.Iframe: true,
	atom.Form: true, atom.Button: true, atom.Input: true, atom.Select: true,
	atom.Textarea: true, atom.Nav: true, atom.Aside: true, atom.Footer: true,
	atom.Header: true, atom.Svg: true, atom.Object: true, atom.Embed: true,
	atom.Template: true, atom.Dialog: true, atom.Head: true, atom.Title: true,
}

func sanitizeChildren(buf *bytes.Buffer, n *html.Node, base *url.URL) {
	for c := n.FirstChild; c != nil; c = c.NextSibling {
		sanitize(buf, c, base)
	}
}

// sanitize writes n as safe HTML: an allowlist of tags, no attributes but
// http(s) links and image sources, so nothing from the page can run
func sanitize(buf *bytes.Buffer, n *html.Node, base *url.URL) {
	switch n.Type {
	case html.TextNode:
		buf.WriteString(html.EscapeString(n.Data))
		return
	case html.ElementNode:
	default:
		sanitizeChildren(buf, n, base)
		return
	}

	if dropTags[n.DataAtom] || junkRegexp.MatchString(attr(n, "class")+" "+attr(n, "id")) {
		return
	}
	tag := n.DataAtom
	if tag == atom.H1 {
		tag = atom.H2
	}
	if !keepTags[tag] {
		sanitizeChildren(buf, n, base)
		return
	}

	switch tag {
	case atom.Img:
		src := attr(n, "src")
		if src == "" || strings.HasPrefix(src, "data:") {
			src = firstNonEmpty(attr(n, "data-src"), attr(n, "data-original"), firstSrcset(attr(n, "srcset")))
		}
		if src = resolveURL(base, src); src != "" {
			fmt.Fprintf(buf, `<img src="%s" alt="%s" loading="lazy">`, html.EscapeString(src), html.EscapeString(attr(n, "alt")))
		}
		return
	case atom.Br, atom.Hr:
		fmt.Fprintf(buf, "<%s>", tag)
		return
	case atom.A:
		if href := resolveURL(base, attr(n, "href")); href != "" {
			fmt.Fprintf(buf, `<a href="%s" rel="noopener noreferrer" target="_blank">`, html.EscapeString(href))
			sanitizeChildren(buf, n, base)
			buf.WriteString("</a>")
			return
		}
		sanitizeChildren(buf, n, base)
		return
	}

	fmt.Fprintf(buf, "<%s>", tag)
	sanitizeChildren(buf, n, base)
	fmt.Fprintf(buf, "</%s>", tag)
}

func resolveURL(base *url.URL, ref string) string {
	ref = strings.TrimSpace(ref)
	if ref == "" {
		return ""
	}
	u, err := base.Parse(ref)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") {
		return ""
	}
	return u.String()
}

func firstSrcset(srcset string) string {
	if f := strings.Fields(srcset); len(f) > 0 {
		return strings.TrimSuffix(f[0], ",")
	}
	return ""
}

// walk visits n and its descendants. visit returns false to skip children
func walk(n *html.Node, visit func(*html.Node) bool) {
	if n.Type == html.ElementNode && !visit(n) {
		return
	}
	for c := n.FirstChild; c != nil; c = c.NextSibling {
		walk(c, visit)
	}
}

func attr(n *html.Node, key string) string {
	for _, a := range n.Attr {
		if a.Key == key {
			return a.Val
		}
	}
	return ""
}

func textOf(n *html.Node) string {
	var sb strings.Builder
	var rec func(*html.Node)
	rec = func(n *html.Node) {
		if n.Type == html.TextNode {
			sb.WriteString(n.Data)
		}
		if n.Type == html.ElementNode && (n.DataAtom == atom.Script || n.DataAtom == atom.Style) {
			return
		}
		for c := n.FirstChild; c != nil; c = c.NextSibling {
			rec(c)
		}
	}
	rec(n)
	return strings.Join(strings.Fields(sb.String()), " ")
}

var tagStripRegexp = regexp.MustCompile(`<[^>]*>`)

func htmlText(s string) string {
	return tagStripRegexp.ReplaceAllString(s, " ")
}

func countWords(s string) int {
	return len(strings.Fields(s))
}

func firstNonEmpty(s ...string) string {
	for _, v := range s {
		if v = strings.TrimSpace(v); v != "" {
			return v
		}
	}
	return ""
}

type readerPage struct {
	Input       string
	Original    string
	Error       string
	Article     *article
	Bookmarklet template.URL
}

func renderReader(w http.ResponseWriter, status int, page readerPage) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	// Article images load from anywhere, nothing else does
	w.Header().Set("Content-Security-Policy", "default-src 'none'; img-src http: https:; style-src 'unsafe-inline'; form-action 'self'; base-uri 'none'")
	w.Header().Set("Referrer-Policy", "no-referrer")
	w.WriteHeader(status)
	if err := readerTemplate.Execute(w, page); err != nil {
		log.Println("[READER]", err)
	}
}

var readerTemplate = template.Must(template.New("reader").Parse(`<!doctype html>
<html lang="en">
<head>
<meta charset="utf-8">
<meta name="viewport" content="width=device-width, initial-scale=1">
<title>{{if .Article}}{{.Article.Title}} · {{end}}FreeWorldNews Reader</title>
<link rel="icon" type="image/svg+xml" href="/favicon.svg">
<style>
:root{--bg:#f5f8f7;--fg:#10262d;--muted:#5e7077;--line:#dce5e3;--accent:#007e73;--card:#fff}
@media (prefers-color-scheme:dark){:root{--bg:#0b171d;--fg:#f2f6f5;--muted:#a0b5b9;--line:#263e44;--accent:#57e3bd;--card:#11272e}}
*{box-sizing:border-box}
body{margin:0;background:var(--bg);color:var(--fg);font:16px/1.5 system-ui,-apple-system,"Segoe UI",sans-serif}
.wrap{max-width:720px;margin:0 auto;padding:0 16px}
header{border-bottom:1px solid var(--line);padding:14px 0;margin-bottom:28px}
header .wrap{display:flex;flex-wrap:wrap;gap:10px;align-items:center}
.brand{display:inline-flex;gap:9px;align-items:center;font-weight:750;font-size:18px;letter-spacing:-.5px;color:var(--fg);text-decoration:none;margin-right:auto}
.brand span{color:var(--accent)}
.brand svg{width:27px;height:27px;color:var(--accent);flex:none}
form{display:flex;gap:8px;width:100%}
input[type=url],input[type=text]{flex:1;min-width:0;font:inherit;padding:10px 12px;border:1px solid var(--line);border-radius:8px;background:var(--card);color:var(--fg)}
button{font:inherit;font-weight:650;padding:10px 16px;border:0;border-radius:8px;background:var(--accent);color:var(--bg);cursor:pointer}
.home{padding:12vh 0 40px;text-align:center}
.home h1{font-size:clamp(2.4rem,7vw,3.6rem);line-height:1.08;letter-spacing:-2px;margin:12px 0 20px}
.home h1 span{color:var(--accent)}
.edition{font-size:.72rem;font-weight:750;letter-spacing:2.4px;color:var(--accent)}
.home p{color:var(--muted);margin:0 0 24px}
.tips{margin-top:40px;text-align:left;color:var(--muted);font-size:.92rem;border-top:1px solid var(--line);padding-top:20px}
.tips a{color:var(--accent)}
.import{border-top:1px solid var(--line);margin-top:24px;padding-top:16px;color:var(--muted)}
.import form{flex-direction:column;margin-top:12px}
.import input[type=file]{max-width:100%;font:inherit}
.import summary{cursor:pointer}
.import label{display:flex;flex-direction:column;gap:6px}
.import a,.error a{color:var(--accent)}
.error{background:var(--card);border:1px solid var(--accent);border-radius:8px;padding:12px 14px;margin:0 0 20px}
article h1{font:700 2rem/1.2 Georgia,"Times New Roman",serif;margin:0 0 10px}
.meta{color:var(--muted);font-size:.9rem;margin-bottom:24px}
.meta a{color:var(--muted)}
.content{font:1.2rem/1.7 Georgia,"Times New Roman",serif;overflow-wrap:break-word}
.content img{max-width:100%;height:auto;border-radius:6px}
.content figure{margin:24px 0}
.content figcaption{font:.85rem/1.4 system-ui,sans-serif;color:var(--muted);margin-top:6px}
.content a{color:var(--accent)}
.content blockquote{border-left:3px solid var(--line);margin:0;padding-left:16px;color:var(--muted)}
.content table{display:block;overflow-x:auto;border-collapse:collapse;font-size:.95rem}
.content td,.content th{border:1px solid var(--line);padding:4px 8px}
.lead{width:100%;border-radius:8px;margin-bottom:20px}
.short{color:var(--muted);font-style:italic;border-top:1px solid var(--line);padding-top:16px;margin-top:32px}
footer.wrap{color:var(--muted);font-size:.85rem;padding:40px 16px}
</style>
</head>
<body>
<header><div class="wrap">
<a class="brand" href="/" aria-label="FreeWorldNews home"><svg viewBox="0 0 32 32" fill="none" stroke="currentColor" stroke-width="1.8" aria-hidden="true"><circle cx="16" cy="16" r="13"/><ellipse cx="16" cy="16" rx="6" ry="13"/><path d="M3 16h26M6 8.5h20M6 23.5h20"/></svg><b>Free<span>World</span>News</b></a>
{{if or .Article .Error}}<form action="/read" method="get"><input type="text" name="url" placeholder="Paste article link" value="{{.Input}}" required><button>Read</button></form>{{end}}
</div></header>
<main class="wrap">
{{if .Error}}<p class="error">{{.Error}}{{if .Original}} <a href="{{.Original}}" rel="noopener noreferrer" target="_blank">Open original article</a>.{{end}}</p>{{end}}
{{with .Article}}
<article>
<h1>{{.Title}}</h1>
<div class="meta">{{.Site}} · {{.Words}} words from {{.Source}} · <a href="{{.URL}}" rel="noopener noreferrer" target="_blank">Original</a></div>
{{if .Image}}<img class="lead" src="{{.Image}}" alt="">{{end}}
<div class="content">{{.Content}}</div>
{{if lt .Words 150}}<p class="short">Only a short excerpt could be found. If you can read the full article in your browser, use the bookmark or import its saved HTML below.</p>{{end}}
</article>
{{else}}{{if not .Error}}
<section class="home">
<div class="edition">YOUR WORLD. YOUR READER.</div>
<h1>Read the world.<br><span>Skip the clutter.</span></h1>
<p>Paste an article link. Get a calm, readable page.</p>
<form action="/read" method="get"><input type="text" name="url" placeholder="https://asia.nikkei.com/..." autofocus required><button>Read</button></form>
<div class="tips">
<p><b>From your browser:</b> drag <a href="{{.Bookmarklet}}">Read with FreeWorldNews</a> to your bookmarks bar. Open an article, finish any browser verification, wait for the text to load, then click the bookmark.</p>
<p>The bookmark sends the open page's HTML to your FreeWorldNews server for reading. It does not read the site's cookies.</p>
<p>On your phone, add this page to your home screen.</p>
</div>
</section>
{{end}}{{end}}
<details class="import" {{if .Error}}open{{end}}>
<summary>Read a page saved from your browser</summary>
<p>For one-click reading, drag <a href="{{.Bookmarklet}}">Read with FreeWorldNews</a> to your bookmarks bar, then use it on the loaded article.</p>
<p>Open the article in your browser and save it as HTML (Ctrl+S or ⌘S). Import that file here. If the site prevents the bookmark from working, use this form.</p>
<p>Only text present in the saved page can be read. Imported pages are not cached.</p>
<form action="/import" method="post" enctype="multipart/form-data">
<label>Original article link <input type="url" name="url" value="{{.Input}}" placeholder="https://example.com/article" required></label>
<label>Saved HTML file <input type="file" name="file" accept=".html,.htm,text/html" required></label>
<button>Read saved page</button>
</form>
</details>
</main>
<footer class="wrap">Text is fetched by your own FreeWorldNews server.</footer>
</body>
</html>
`))
