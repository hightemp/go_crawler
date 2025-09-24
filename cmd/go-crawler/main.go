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
	flag.StringVar(&cfgPath, "config", "config.yaml", "Путь к YAML конфигурации")
	flag.Parse()

	cfg, err := loadConfig(cfgPath)
	if err != nil {
		log.Fatalf("Ошибка загрузки конфига: %v", err)
	}

	timeout := defaultTimeoutSec
	if cfg.HTTP.TimeoutSec > 0 {
		timeout = cfg.HTTP.TimeoutSec
	}
	client := &http.Client{
		Timeout: time.Duration(timeout) * time.Second,
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
			log.Printf("Пропуск job #%d (disabled)", i)
			continue
		}
		if err := runJob(job, client, userAgent, retries, backoffMs); err != nil {
			log.Printf("Ошибка выполнения job #%d: %v", i, err)
		}
	}
}
