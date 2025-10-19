package main

import (
	"context"
	"log"
	"net"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	xproxy "golang.org/x/net/proxy"
)

type proxyType int

const (
	proxyHTTP proxyType = iota
	proxySOCKS5
)

type proxyEntry struct {
	raw string
	url *url.URL
	typ proxyType
}

// ProxyPoolTransport — RoundTripper, который ротирует список прокси и
// проксирует запросы через выбранный транспорт (HTTP/HTTPS proxy или SOCKS5).
type ProxyPoolTransport struct {
	entries     []proxyEntry
	transports  map[string]*http.Transport
	transMu     sync.RWMutex
	rr          uint64
	dialTimeout time.Duration
}

// NewProxyPoolTransport строит RoundTripper, который использует список прокси.
// Поддерживаемые схемы: http, https, socks5, socks5h.
// Если список пуст или невалиден — возвращается (nil, nil), что сигнализирует использовать стандартный транспорт.
func NewProxyPoolTransport(proxies []string, timeoutSec int) (http.RoundTripper, error) {
	entries := make([]proxyEntry, 0, len(proxies))
	for _, raw := range proxies {
		s := strings.TrimSpace(raw)
		if s == "" {
			continue
		}
		u, err := url.Parse(s)
		if err != nil {
			log.Printf("Invalid proxy URL ignored: %s (%v)", raw, err)
			continue
		}
		scheme := strings.ToLower(u.Scheme)
		var typ proxyType
		switch scheme {
		case "http", "https":
			typ = proxyHTTP
		case "socks5", "socks5h":
			typ = proxySOCKS5
		default:
			log.Printf("Unsupported proxy scheme ignored: %s", raw)
			continue
		}
		entries = append(entries, proxyEntry{
			raw: s,
			url: u,
			typ: typ,
		})
	}

	if len(entries) == 0 {
		return nil, nil
	}

	p := &ProxyPoolTransport{
		entries:     entries,
		transports:  make(map[string]*http.Transport, len(entries)),
		dialTimeout: time.Duration(timeoutSec) * time.Second,
	}
	return p, nil
}

func (p *ProxyPoolTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	if p == nil || len(p.entries) == 0 {
		return http.DefaultTransport.RoundTrip(req)
	}
	idx := int(atomic.AddUint64(&p.rr, 1)-1) % len(p.entries)
	entry := p.entries[idx]
	tr := p.getTransport(entry)
	return tr.RoundTrip(req)
}

func (p *ProxyPoolTransport) getTransport(entry proxyEntry) *http.Transport {
	p.transMu.RLock()
	t := p.transports[entry.raw]
	p.transMu.RUnlock()
	if t != nil {
		return t
	}

	p.transMu.Lock()
	defer p.transMu.Unlock()
	if t = p.transports[entry.raw]; t != nil {
		return t
	}

	t = buildTransport(entry, p.dialTimeout)
	p.transports[entry.raw] = t
	return t
}

func buildTransport(entry proxyEntry, dialTimeout time.Duration) *http.Transport {
	baseDialer := &net.Dialer{
		Timeout:   dialTimeout,
		KeepAlive: 30 * time.Second,
	}

	tr := &http.Transport{
		Proxy:                 nil, // set below if HTTP proxy
		DialContext:           baseDialer.DialContext,
		ForceAttemptHTTP2:     true,
		MaxConnsPerHost:       64,
		MaxIdleConns:          512,
		MaxIdleConnsPerHost:   64,
		IdleConnTimeout:       90 * time.Second,
		TLSHandshakeTimeout:   10 * time.Second,
		ExpectContinueTimeout: 1 * time.Second,
		ResponseHeaderTimeout: 15 * time.Second,
	}

	switch entry.typ {
	case proxyHTTP:
		tr.Proxy = http.ProxyURL(entry.url)
	case proxySOCKS5:
		// socks5://user:pass@host:port
		var auth *xproxy.Auth
		if entry.url.User != nil {
			user := entry.url.User.Username()
			pass, _ := entry.url.User.Password()
			auth = &xproxy.Auth{
				User:     user,
				Password: pass,
			}
		}
		addr := entry.url.Host
		if !strings.Contains(addr, ":") {
			addr = net.JoinHostPort(addr, "1080")
		}
		d, err := xproxy.SOCKS5("tcp", addr, auth, baseDialer)
		if err != nil {
			log.Printf("Failed to configure SOCKS5 proxy %s: %v", entry.raw, err)
			// Fallback: оставим базовый транспорт без прокси
			return tr
		}
		tr.Proxy = nil
		tr.DialContext = func(ctx context.Context, network, address string) (net.Conn, error) {
			return d.Dial(network, address)
		}
	}
	return tr
}

// NewDefaultTransport returns a tuned http.RoundTripper without a proxy.
func NewDefaultTransport(timeoutSec int) http.RoundTripper {
	baseDialer := &net.Dialer{
		Timeout:   time.Duration(timeoutSec) * time.Second,
		KeepAlive: 30 * time.Second,
	}
	return &http.Transport{
		Proxy:                 nil,
		DialContext:           baseDialer.DialContext,
		ForceAttemptHTTP2:     true,
		MaxConnsPerHost:       64,
		MaxIdleConns:          512,
		MaxIdleConnsPerHost:   64,
		IdleConnTimeout:       90 * time.Second,
		TLSHandshakeTimeout:   10 * time.Second,
		ExpectContinueTimeout: 1 * time.Second,
		ResponseHeaderTimeout: 15 * time.Second,
	}
}
