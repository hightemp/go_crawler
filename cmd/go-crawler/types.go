package main

import (
	"net/url"

	"golang.org/x/net/html"
)

// Конфигурация приложения
type Config struct {
	Items []Job      `yaml:"items"`
	HTTP  HTTPConfig `yaml:"http"`
}

type HTTPConfig struct {
	TimeoutSec     int    `yaml:"timeout_sec"`
	UserAgent      string `yaml:"user_agent"`
	MaxRetries     int    `yaml:"max_retries"`
	RetryBackoffMs int    `yaml:"retry_backoff_ms"`
}

// Описание задания
type Job struct {
	Enabled       bool     `yaml:"enabled"`
	Type          string   `yaml:"type"` // "page" | "pages" | "site"
	Urls          []string `yaml:"urls"`
	OutputDir     string   `yaml:"output_dir"`
	IncludeAssets bool     `yaml:"include_assets"`
	AssetTypes    []string `yaml:"asset_types"` // css, js, img, font, media
	SameHostOnly  bool     `yaml:"same_host_only"`
	MaxDepth      int      `yaml:"max_depth"` // 0 - без ограничения
	MaxPages      int      `yaml:"max_pages"` // 0 - без ограничения
}

type crawlQueueItem struct {
	u     *url.URL
	depth int
}

type assetRefKind string

const (
	assetCSS   assetRefKind = "css"
	assetJS    assetRefKind = "js"
	assetIMG   assetRefKind = "img"
	assetFONT  assetRefKind = "font"
	assetMEDIA assetRefKind = "media"
	assetOTHER assetRefKind = "other"
)

// Ссылка на ассет в DOM c доп. информацией
type assetRef struct {
	node      *html.Node
	attrIndex int
	absURL    *url.URL
	kind      assetRefKind
	// srcset
	isSrcset bool
	desc     string // дескриптор srcset (например, "1x", "480w")
}
