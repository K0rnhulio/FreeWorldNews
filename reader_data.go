package main

import (
	"encoding/json"
	"html"
	"net/url"
	"regexp"
	"strings"
)

var fusionAssignment = regexp.MustCompile(`\bFusion\.globalContent\s*=\s*`)

type articleDataScript struct {
	data   string
	fusion bool
}

// Fusion embeds article data in executable script tags rather than JSON data
// tags. Decode the JSON value only; never execute JavaScript from a publisher.
func fusionArticleData(script string, base *url.URL) []string {
	var payloads []string
	for _, match := range fusionAssignment.FindAllStringIndex(script, 16) {
		var raw json.RawMessage
		decoder := json.NewDecoder(strings.NewReader(script[match[1]:]))
		if decoder.Decode(&raw) != nil || len(raw) == 0 || raw[0] != '{' {
			continue
		}
		var data map[string]interface{}
		if json.Unmarshal(raw, &data) != nil || !matchesArticleData(data, base) {
			continue
		}
		payloads = append(payloads, string(raw))
	}
	return payloads
}

func matchesArticleData(data map[string]interface{}, base *url.URL) bool {
	for _, key := range []string{"canonicalUrl", "canonical_url"} {
		if canonical, _ := data[key].(string); canonical != "" {
			u, err := base.Parse(canonical)
			return err == nil && strings.EqualFold(strings.TrimPrefix(u.Hostname(), "www."), strings.TrimPrefix(base.Hostname(), "www.")) &&
				strings.TrimRight(u.Path, "/") == strings.TrimRight(base.Path, "/")
		}
	}
	if id, _ := data["_id"].(string); id != "" {
		return strings.Contains(base.Path, id)
	}
	return true
}

// Block arrays must be joined in order. Selecting the longest individual text
// field discards most of an article, even when the whole body was delivered.
func structuredArticleBody(s string, base *url.URL) string {
	var value interface{}
	if json.Unmarshal([]byte(s), &value) != nil {
		return ""
	}
	best, bestWords := "", 0
	var visit func(interface{})
	visit = func(value interface{}) {
		switch value := value.(type) {
		case map[string]interface{}:
			if !matchesArticleData(value, base) {
				return
			}
			for _, key := range []string{"elements", "content_elements"} {
				if blocks, ok := value[key].([]interface{}); ok {
					body := renderArticleBlocks(blocks, 0)
					if words := countWords(htmlText(body)); words > bestWords {
						best, bestWords = body, words
					}
				}
			}
			for _, child := range value {
				visit(child)
			}
		case []interface{}:
			for _, child := range value {
				visit(child)
			}
		}
	}
	visit(value)
	return best
}

// This produces a candidate body, which still passes through the normal HTML
// sanitizer before being shown. Non-editorial blocks are deliberately omitted.
func renderArticleBlocks(blocks []interface{}, depth int) string {
	if depth > 16 {
		return ""
	}
	var body strings.Builder
	for _, block := range blocks {
		if text, ok := block.(string); ok {
			body.WriteString("<p>" + text + "</p>\n")
			if body.Len() > maxPageBytes {
				return ""
			}
			continue
		}
		block, ok := block.(map[string]interface{})
		if !ok {
			continue
		}
		kind, _ := block["type"].(string)
		text, _ := block["text"].(string)
		if text == "" {
			text, _ = block["content"].(string)
		}
		switch kind {
		case "text", "paragraph":
			body.WriteString("<p>" + text + "</p>\n")
		case "header", "heading":
			body.WriteString("<h2>" + text + "</h2>\n")
		case "quote":
			children, _ := block["elements"].([]interface{})
			if children == nil {
				children, _ = block["content_elements"].([]interface{})
			}
			quote := renderArticleBlocks(children, depth+1)
			if quote == "" && text != "" {
				quote = "<p>" + text + "</p>"
			}
			if quote != "" {
				body.WriteString("<blockquote>" + quote)
				if author, _ := block["author"].(string); author != "" {
					body.WriteString("<p>" + html.EscapeString(author) + "</p>")
				}
				body.WriteString("</blockquote>\n")
			}
		}
		if body.Len() > maxPageBytes {
			return ""
		}
	}
	return body.String()
}
