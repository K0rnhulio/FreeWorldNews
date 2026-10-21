package main

import (
	"encoding/json"
	"net/http"
	"net/url"
	"os"
	"strings"
	"testing"
)

func TestFusionArticleBlocks(t *testing.T) {
	base, _ := url.Parse("https://news.example.com/story-ID.html")
	data := map[string]interface{}{
		"canonicalUrl": "/story-ID.html",
		"elements": []interface{}{
			map[string]interface{}{"type": "piano"},
			map[string]interface{}{"type": "text", "text": "FIRST " + words(100) + ` <a href="/details" onclick="alert(1)">Details</a><script>alert(1)</script>`},
			map[string]interface{}{"type": "ad", "text": "ADVERTISING " + words(800)},
			map[string]interface{}{"type": "header", "text": "Middle heading"},
			map[string]interface{}{"type": "quote", "author": "A & B", "elements": []interface{}{"Quoted words remain in order"}},
			map[string]interface{}{"type": "text", "text": "LAST " + words(120)},
		},
	}
	raw, _ := json.Marshal(data)
	page := `<html><head><title>Fusion story</title></head><body><article><p>` + words(30) + `</p></article><script>window.Fusion={};Fusion.globalContent = ` + string(raw) + `;Fusion.globalContentConfig={};throw new Error("must never execute");</script></body></html>`
	a := extractArticle([]byte(page), base)
	body := string(a.Content)
	if a.Words < 220 || a.Words > 250 || !strings.Contains(body, "LAST") || strings.Index(body, "FIRST") > strings.Index(body, "LAST") {
		t.Fatalf("word count/order: words=%d content=%s", a.Words, body)
	}
	for _, bad := range []string{"ADVERTISING", "onclick", "<script", "alert(1)", "throw new Error"} {
		if strings.Contains(body, bad) {
			t.Errorf("unsafe or non-editorial content %q", bad)
		}
	}
	for _, want := range []string{`href="https://news.example.com/details"`, "<blockquote>", "<h2>Middle heading</h2>", "A &amp; B"} {
		if !strings.Contains(body, want) {
			t.Errorf("missing %q", want)
		}
	}
}

func TestFusionRejectsUnrelatedOrExecutableData(t *testing.T) {
	base, _ := url.Parse("https://news.example.com/story-ID.html")
	for _, script := range []string{
		`Fusion.globalContent = {"canonicalUrl":"/different-story.html","elements":[{"type":"text","text":"` + words(300) + `"}]};`,
		`Fusion.globalContent = {"canonicalUrl":"https://another.example/story-ID.html","elements":[{"type":"text","text":"` + words(300) + `"}]};`,
		`Fusion.globalContent = fetch("https://example.com/payload");`,
		`Fusion.globalContent = {invalidJSON:true};`,
	} {
		page := `<article><p>` + words(30) + `</p></article><script>` + script + `</script>`
		if a := extractArticle([]byte(page), base); a.Words != 30 {
			t.Fatalf("unrelated or executable payload replaced article: %d", a.Words)
		}
	}
}

func TestStructuredContentElements(t *testing.T) {
	base, _ := url.Parse("https://example.com/story")
	raw, _ := json.Marshal(map[string]interface{}{"content_elements": []interface{}{
		map[string]interface{}{"type": "text", "content": words(100)},
		map[string]interface{}{"type": "text", "content": words(100)},
	}})
	a := extractArticle([]byte(`<script type="application/json">`+string(raw)+`</script>`), base)
	if a.Words != 200 {
		t.Fatalf("must combine all article paragraphs, words=%d", a.Words)
	}
}

func TestArchivedFusionUsesOriginalURL(t *testing.T) {
	base, _ := url.Parse("https://example.com/story")
	final, _ := url.Parse("https://web.archive.org/web/20261021000000id_/https://example.com/story")
	raw, _ := json.Marshal(map[string]interface{}{"canonicalUrl": "/story", "elements": []interface{}{map[string]interface{}{"type": "text", "text": words(200)}}})
	a, err := extractFetchedPage([]byte(`<script>Fusion.globalContent=`+string(raw)+`;</script>`), base, final, 200, http.Header{}, modeArchive)
	if err != nil || a.Words != 200 {
		t.Fatalf("archive article=%+v err=%v", a, err)
	}
}

func TestArticleSnapshot(t *testing.T) {
	path := readerTestEnv("READER_SNAPSHOT")
	if path == "" {
		t.Skip("set FREEWORLDNEWS_READER_SNAPSHOT and FREEWORLDNEWS_BROWSER_TEST_URL to inspect a saved page")
	}
	base, err := url.Parse(readerTestEnv("BROWSER_TEST_URL"))
	if err != nil || !validArticleURL(base) {
		t.Fatal("a valid original article URL is required")
	}
	page, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	a := extractArticle(page, base)
	t.Logf("snapshot title=%q words=%d", a.Title, a.Words)
	if a.Words < minArticleLen {
		t.Fatal("snapshot did not yield a substantial article")
	}
}
