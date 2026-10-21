package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"
)

var readerRenderer func(context.Context, *url.URL) (*article, error)

type commandBrowser struct {
	node, script, channel string
	headless              bool
	slots                 chan struct{}
	dial                  func(context.Context, string, string) (net.Conn, error)
	screenshot            string
	diagnostics           string
}

func newCommandBrowser(node, script, channel string, headful bool) (*commandBrowser, error) {
	node, err := exec.LookPath(node)
	if err != nil {
		return nil, fmt.Errorf("browser fallback needs Node.js: %w", err)
	}
	script, err = filepath.Abs(script)
	if err != nil {
		return nil, err
	}
	if _, err := os.Stat(script); err != nil {
		return nil, fmt.Errorf("browser worker: %w", err)
	}
	if channel != "chromium" && channel != "chrome" && channel != "msedge" {
		return nil, errors.New("readerBrowserChannel must be chromium, chrome or msedge")
	}
	return &commandBrowser{node: node, script: script, channel: channel, headless: !headful, slots: make(chan struct{}, 1)}, nil
}

func (b *commandBrowser) fetch(ctx context.Context, u *url.URL) (*article, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	ctx, cancel := context.WithTimeout(ctx, 35*time.Second)
	defer cancel()
	select {
	case b.slots <- struct{}{}:
		defer func() { <-b.slots }()
	case <-ctx.Done():
		return nil, ctx.Err()
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	dial := b.dial
	if dial == nil {
		dial = newReaderTransport().DialContext
	}
	proxy, err := newBrowserProxy(ctx, dial)
	if err != nil {
		return nil, err
	}
	defer proxy.close()
	cmd := exec.Command(b.node, b.script)
	prepareBrowserProcess(cmd)
	stdin, err := cmd.StdinPipe()
	if err != nil {
		return nil, err
	}
	defer stdin.Close()
	out := &boundedOutput{limit: 6*maxPageBytes + (64 << 10)}
	stderr := &boundedOutput{limit: 64 << 10}
	cmd.Stdout, cmd.Stderr = out, stderr
	if err := cmd.Start(); err != nil {
		return nil, fmt.Errorf("couldn't start browser worker: %w", err)
	}
	input := struct {
		URL         string        `json:"url"`
		Proxy       *browserProxy `json:"proxy"`
		Channel     string        `json:"channel"`
		Headless    bool          `json:"headless"`
		TimeoutMS   int           `json:"timeoutMs"`
		Screenshot  string        `json:"screenshotPath,omitempty"`
		Diagnostics string        `json:"diagnosticsDir,omitempty"`
	}{u.String(), proxy, b.channel, b.headless, 28000, b.screenshot, b.diagnostics}
	if err := json.NewEncoder(stdin).Encode(input); err != nil {
		killBrowserProcess(cmd)
		cmd.Wait()
		return nil, err
	}
	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()
	select {
	case err = <-done:
	case <-ctx.Done():
		// EOF asks the worker to close its browser and temporary profile first.
		stdin.Close()
		select {
		case <-done:
		case <-time.After(3 * time.Second):
			killBrowserProcess(cmd)
			<-done
		}
		return nil, ctx.Err()
	}
	if err != nil {
		log.Printf("[BROWSER] Worker failed: %v: %s", err, strings.TrimSpace(stderr.String()))
		return nil, errors.New("the browser worker couldn't start; check its installation and server logs")
	}
	var result struct {
		URL     string            `json:"url"`
		Status  int               `json:"status"`
		Headers map[string]string `json:"headers"`
		HTML    string            `json:"html"`
		Error   string            `json:"error"`
	}
	if err := json.Unmarshal(out.Bytes(), &result); err != nil {
		return nil, errors.New("invalid browser worker response")
	}
	if result.Error != "" {
		return nil, fmt.Errorf("browser rendering failed: %s", result.Error)
	}
	final, err := url.Parse(result.URL)
	if err != nil || !validArticleURL(final) {
		return nil, errors.New("the browser returned an invalid URL")
	}
	h := make(http.Header)
	for key, value := range result.Headers {
		h.Set(key, value)
	}
	return extractFetchedPage([]byte(result.HTML), u, final, result.Status, h, modeRendered)
}

type boundedOutput struct {
	bytes.Buffer
	limit int
}

func (b *boundedOutput) Write(p []byte) (int, error) {
	if len(p) > b.limit-b.Len() {
		return 0, errors.New("browser worker output is too large")
	}
	return b.Buffer.Write(p)
}
