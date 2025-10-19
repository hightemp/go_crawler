package main

import (
	"flag"
	"log"
	"net/http"
	"strings"
	"time"
)

const (
	defaultTimeoutSec = 20
	defaultUserAgent  = "go_crawler/0.1 (+https://example.local)"
)

func main() {
	var cfgPath string
	flag.StringVar(&cfgPath, "config", "config.yaml", "Path to YAML configuration")
	flag.Parse()

	cfg, err := loadConfig(cfgPath)
	if err != nil {
		log.Fatalf("Failed to load config: %v", err)
	}

	timeout := defaultTimeoutSec
	if cfg.HTTP.TimeoutSec > 0 {
		timeout = cfg.HTTP.TimeoutSec
	}

	// Build HTTP client with optional proxy rotation transport
	tr, perr := NewProxyPoolTransport(cfg.HTTP.Proxies, timeout)
	if perr != nil {
		log.Printf("Proxy configuration error, using direct connection: %v", perr)
	}

	client := &http.Client{
		Timeout: time.Duration(timeout) * time.Second,
	}
	if tr != nil {
		client.Transport = tr
	}

	userAgent := cfg.HTTP.UserAgent
	if strings.TrimSpace(userAgent) == "" {
		userAgent = defaultUserAgent
	}

	retries := cfg.HTTP.MaxRetries
	if retries < 0 {
		retries = 0
	}
	backoffMs := cfg.HTTP.RetryBackoffMs
	if backoffMs <= 0 {
		backoffMs = 250
	}

	for i, job := range cfg.Items {
		if !job.Enabled {
			log.Printf("Skipping job #%d (disabled)", i)
			continue
		}
		if err := runJob(job, client, userAgent, retries, backoffMs); err != nil {
			log.Printf("Error running job #%d: %v", i, err)
		}
	}
}
