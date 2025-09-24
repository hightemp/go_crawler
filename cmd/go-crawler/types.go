package main

import (
	"net/url"

	"golang.org/x/net/html"
)

// App configuration
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

// Job description
type Job struct {
	Enabled       bool     `yaml:"enabled"`
	Type          string   `yaml:"type"` // "page" | "pages" | "site"
	Urls          []string `yaml:"urls"`
	OutputDir     string   `yaml:"output_dir"`
	IncludeAssets bool     `yaml:"include_assets"`
	AssetTypes    []string `yaml:"asset_types"` // css, js, img, font, media
	SameHostOnly  bool     `yaml:"same_host_only"`
	MaxDepth      int      `yaml:"max_depth"` // 0 - no limit
	MaxPages      int      `yaml:"max_pages"` // 0 - no limit
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

// Reference to an asset in the DOM with extra info
type assetRef struct {
	node      *html.Node
	attrIndex int
	absURL    *url.URL
	kind      assetRefKind
	// srcset
	isSrcset bool
	desc     string // srcset descriptor (e.g., "1x", "480w")
}
