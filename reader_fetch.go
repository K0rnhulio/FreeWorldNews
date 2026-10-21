package main

import (
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"
)

var errNoArchive = errors.New("the Wayback Machine has no copy")
var readerHistory = newReaderMemory()

const (
	readerCacheBytes = 4 << 20
	readerStateLimit = 256
)

type modeMemory struct {
	mode  fetchMode
	until time.Time
}

type pauseMemory struct {
	err   error
	until time.Time
}

type cachedArticle struct {
	article article
	until   time.Time
	size    int
}

// All state is bounded, in memory, and expires. No browser session is stored.
type readerMemory struct {
	mu        sync.Mutex
	now       func() time.Time
	preferred map[string]modeMemory
	paused    map[string]pauseMemory
	articles  map[string]cachedArticle
	bytes     int
}

func newReaderMemory() *readerMemory {
	return &readerMemory{
		now:       time.Now,
		preferred: make(map[string]modeMemory),
		paused:    make(map[string]pauseMemory),
		articles:  make(map[string]cachedArticle),
	}
}

func (m *readerMemory) modes(host string, archiveFirst bool) []fetchMode {
	m.mu.Lock()
	defer m.mu.Unlock()
	first := modeBrowser
	if p, ok := m.preferred[strings.ToLower(host)]; ok && m.now().Before(p.until) {
		first = p.mode
	}
	if first == modeRendered && readerRenderer == nil {
		first = modeBrowser
	}
	if archiveFirst {
		first = modeArchive
	}
	modes := []fetchMode{first}
	for _, mode := range []fetchMode{modeBrowser, modeRendered, modeTricks, modeArchive} {
		if mode == modeRendered && readerRenderer == nil {
			continue
		}
		if mode != first {
			modes = append(modes, mode)
		}
	}
	return modes
}

func (m *readerMemory) rememberMode(host string, mode fetchMode) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if len(m.preferred) >= readerStateLimit {
		for key := range m.preferred {
			delete(m.preferred, key)
			break
		}
	}
	m.preferred[strings.ToLower(host)] = modeMemory{mode, m.now().Add(30 * time.Minute)}
}

func (m *readerMemory) cooldown(host string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	host = strings.ToLower(host)
	p := m.paused[host]
	if m.now().Before(p.until) {
		return p.err
	}
	delete(m.paused, host)
	return nil
}

func (m *readerMemory) pause(host string, until time.Time, err error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	host = strings.ToLower(host)
	if old := m.paused[host]; old.until.After(until) {
		return
	}
	if len(m.paused) >= readerStateLimit {
		for key := range m.paused {
			delete(m.paused, key)
			break
		}
	}
	m.paused[host] = pauseMemory{err, until}
}

func (m *readerMemory) cached(key string) *article {
	m.mu.Lock()
	defer m.mu.Unlock()
	v, ok := m.articles[key]
	if !ok {
		return nil
	}
	if !m.now().Before(v.until) {
		m.bytes -= v.size
		delete(m.articles, key)
		return nil
	}
	a := v.article
	return &a
}

func (m *readerMemory) cache(key string, a *article) {
	if a.Words < minArticleLen || !a.cacheable {
		return
	}
	size := len(key) + len(a.Content) + len(a.URL) + len(a.Image) + len(a.Title) + len(a.Site)
	if size > readerCacheBytes {
		return
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if old, ok := m.articles[key]; ok {
		m.bytes -= old.size
		delete(m.articles, key)
	}
	for len(m.articles) >= 64 || m.bytes+size > readerCacheBytes {
		var oldest string
		var until time.Time
		for k, v := range m.articles {
			if until.IsZero() || v.until.Before(until) {
				oldest, until = k, v.until
			}
		}
		m.bytes -= m.articles[oldest].size
		delete(m.articles, oldest)
	}
	m.articles[key] = cachedArticle{*a, m.now().Add(a.cacheTTL), size}
	m.bytes += size
}

type upstreamCooldownError struct {
	Host  string
	Until time.Time
}

func (e *upstreamCooldownError) Error() string {
	return fmt.Sprintf("%s asked readers to slow down; try again after %s", e.Host, e.Until.UTC().Format(time.RFC1123))
}

func retryAfter(value string, now time.Time) time.Time {
	if seconds, err := strconv.ParseInt(strings.TrimSpace(value), 10, 64); err == nil && seconds >= 0 {
		// Bound untrusted values before converting seconds to a duration.
		if seconds > 86400 {
			seconds = 86400
		}
		return now.Add(time.Duration(seconds) * time.Second)
	}
	if until, err := http.ParseTime(value); err == nil && until.After(now) {
		return until
	}
	return now.Add(time.Minute)
}

func articleCacheTTL(h http.Header, now time.Time) time.Duration {
	ttl := 5 * time.Minute
	age, _ := strconv.ParseInt(h.Get("Age"), 10, 32)
	if age < 0 {
		age = 0
	}
	for _, directive := range strings.Split(strings.ToLower(strings.Join(h.Values("Cache-Control"), ",")), ",") {
		parts := strings.SplitN(strings.TrimSpace(directive), "=", 2)
		switch strings.TrimSpace(parts[0]) {
		case "private", "no-store", "no-cache":
			return 0
		case "max-age", "s-maxage":
			if len(parts) != 2 {
				return 0
			}
			seconds, err := strconv.ParseInt(strings.Trim(strings.TrimSpace(parts[1]), `"`), 10, 32)
			if err != nil || seconds <= age {
				return 0
			}
			if d := time.Duration(seconds-age) * time.Second; d < ttl {
				ttl = d
			}
		}
	}
	if expires, err := http.ParseTime(h.Get("Expires")); err == nil && expires.Sub(now) < ttl {
		ttl = expires.Sub(now)
	}
	if strings.Contains(strings.Join(h.Values("Vary"), ","), "*") || ttl < 0 {
		return 0
	}
	return ttl
}
