package main

import (
	"context"
	"crypto/rand"
	"crypto/subtle"
	"encoding/base64"
	"errors"
	"io"
	"net"
	"net/http"
	"strings"
	"time"
)

type browserProxy struct {
	Server   string `json:"server"`
	Username string `json:"username"`
	Password string `json:"password"`
	close    func()
}

// Chromium sends every web request through this authenticated loopback proxy.
// The reader dialer checks the actual resolved IP at connection time, including
// subresources and redirects, rather than trusting an earlier DNS lookup.
func newBrowserProxy(ctx context.Context, dial func(context.Context, string, string) (net.Conn, error)) (*browserProxy, error) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return nil, err
	}
	secret := make([]byte, 32)
	if _, err := rand.Read(secret); err != nil {
		ln.Close()
		return nil, err
	}
	p := &browserProxy{Server: "http://" + ln.Addr().String(), Username: "freeworldnews", Password: base64.RawURLEncoding.EncodeToString(secret)}
	wantAuth := "Basic " + base64.StdEncoding.EncodeToString([]byte(p.Username+":"+p.Password))
	tr := &http.Transport{DialContext: dial, DisableKeepAlives: true, ResponseHeaderTimeout: 15 * time.Second}
	server := &http.Server{ReadHeaderTimeout: 5 * time.Second}
	server.Handler = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if subtle.ConstantTimeCompare([]byte(r.Header.Get("Proxy-Authorization")), []byte(wantAuth)) != 1 {
			w.Header().Set("Proxy-Authenticate", `Basic realm="FreeWorldNews browser"`)
			http.Error(w, "Proxy authentication required", http.StatusProxyAuthRequired)
			return
		}
		target := r.URL.Host
		if r.Method == http.MethodConnect {
			target = r.Host
		}
		address, err := browserProxyAddress(target, r.Method == http.MethodConnect)
		if err != nil || (r.Method != http.MethodConnect && (r.URL.Scheme != "http" || r.URL.User != nil)) {
			http.Error(w, "Destination blocked", http.StatusForbidden)
			return
		}
		if r.Method != http.MethodConnect {
			req := r.Clone(ctx)
			req.RequestURI = ""
			req.Header.Del("Proxy-Authorization")
			req.Header.Del("Proxy-Connection")
			res, err := tr.RoundTrip(req)
			if err != nil {
				http.Error(w, "Destination unavailable or blocked", http.StatusBadGateway)
				return
			}
			defer res.Body.Close()
			for key, values := range res.Header {
				for _, value := range values {
					w.Header().Add(key, value)
				}
			}
			w.WriteHeader(res.StatusCode)
			io.Copy(w, res.Body)
			return
		}
		upstream, err := dial(ctx, "tcp", address)
		if err != nil {
			http.Error(w, "Destination unavailable or blocked", http.StatusBadGateway)
			return
		}
		defer upstream.Close()
		client, buffered, err := w.(http.Hijacker).Hijack()
		if err != nil {
			return
		}
		defer client.Close()
		if _, err := buffered.WriteString("HTTP/1.1 200 Connection Established\r\n\r\n"); err != nil {
			return
		}
		if err := buffered.Flush(); err != nil {
			return
		}
		client.SetDeadline(time.Now().Add(40 * time.Second))
		upstream.SetDeadline(time.Now().Add(40 * time.Second))
		done := make(chan struct{})
		defer close(done)
		go func() {
			select {
			case <-ctx.Done():
				client.Close()
				upstream.Close()
			case <-done:
			}
		}()
		go func() {
			io.Copy(upstream, buffered)
			upstream.Close()
		}()
		io.Copy(client, upstream)
	})
	p.close = func() { server.Close(); tr.CloseIdleConnections() }
	go server.Serve(ln)
	return p, nil
}

func browserProxyAddress(target string, tunnel bool) (string, error) {
	host, port, err := net.SplitHostPort(target)
	if err != nil {
		if tunnel {
			return "", err
		}
		host, port = target, "80"
	}
	if host == "" || strings.ContainsAny(host, "@/\\%") || (port != "80" && port != "443") {
		return "", errors.New("unsupported browser destination")
	}
	if ip := net.ParseIP(host); ip != nil && !isPublicIP(ip) {
		return "", errPrivateAddress
	}
	return net.JoinHostPort(host, port), nil
}
