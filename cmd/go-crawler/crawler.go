package main

import (
	"bytes"
	"crypto/sha1"
	"encoding/hex"
	"fmt"
	"io"
	"log"
	"math/rand"
	"mime"
	"net/http"
	"net/url"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"golang.org/x/net/html"
)

// Job runner and crawling core

func runJob(job Job, client *http.Client, ua string, retries int, backoffMs int, workers int, assetWorkers int, maxBodySize int64) error {
	if len(job.Urls) == 0 {
		return fmt.Errorf("job without urls")
	}
	if strings.TrimSpace(job.OutputDir) == "" {
		return fmt.Errorf("job without output_dir")
	}
	job.Type = strings.ToLower(strings.TrimSpace(job.Type))
	if job.Type == "" {
		job.Type = "page"
	}
	// defaults
	if job.IncludeAssets && len(job.AssetTypes) == 0 {
		job.AssetTypes = []string{"css", "js", "img"}
	}
	assetAllowed := make(map[string]bool)
	for _, t := range job.AssetTypes {
		assetAllowed[strings.ToLower(strings.TrimSpace(t))] = true
	}

	if err := os.MkdirAll(job.OutputDir, 0o755); err != nil {
		return fmt.Errorf("create output directory: %w", err)
	}

	// allowed hosts
	allowedHosts := map[string]bool{}
	for _, s := range job.Urls {
		if u := parseURL(s); u != nil {
			allowedHosts[strings.ToLower(u.Hostname())] = true
		}
	}

	switch job.Type {
	case "page":
		// Параллельная обработка списка отдельных страниц
		if workers <= 0 {
			workers = 1
		}
		visitedAssets := &sync.Map{}
		var wg sync.WaitGroup
		sem := make(chan struct{}, workers)
		for _, s := range job.Urls {
			u := parseURL(s)
			if u == nil {
				log.Printf("Skipping invalid URL: %s", s)
				continue
			}
			wg.Add(1)
			sem <- struct{}{}
			go func(u *url.URL) {
				defer wg.Done()
				defer func() { <-sem }()
				if err := crawlSinglePage(u, job, client, ua, retries, backoffMs, visitedAssets, assetAllowed, assetWorkers, maxBodySize); err != nil {
					log.Printf("Error downloading page %s: %v", u.String(), err)
				}
			}(u)
		}
		wg.Wait()
	case "pages", "site":
		if workers <= 0 {
			workers = 1
		}
		if err := bfsCrawl(job, client, ua, retries, backoffMs, assetAllowed, allowedHosts, workers, assetWorkers, maxBodySize); err != nil {
			return err
		}
	default:
		return fmt.Errorf("unknown job type: %s", job.Type)
	}
	return nil
}

// Crawling

func crawlSinglePage(u *url.URL, job Job, client *http.Client, ua string, retries int, backoffMs int, visitedAssets *sync.Map, assetAllowed map[string]bool, assetWorkers int, maxBodySize int64) error {
	_, err := fetchProcessAndSaveHTML(u, job, client, ua, retries, backoffMs, visitedAssets, assetAllowed, assetWorkers, maxBodySize)
	return err
}

func bfsCrawl(job Job, client *http.Client, ua string, retries int, backoffMs int, assetAllowed map[string]bool, allowedHosts map[string]bool, workers int, assetWorkers int, maxBodySize int64) error {
	jobs := make(chan crawlQueueItem, 1024)
	var visited sync.Map
	visitedAssets := &sync.Map{}

	var pagesSaved atomic.Int64
	var stop int32

	var inFlight sync.WaitGroup
	// Close jobs channel when all enqueued work is drained
	go func() {
		inFlight.Wait()
		close(jobs)
	}()

	// Seed initial URLs
	for _, s := range job.Urls {
		if u := parseURL(s); u != nil {
			inFlight.Add(1)
			jobs <- crawlQueueItem{u: u, depth: 0}
		}
	}

	var workersWG sync.WaitGroup
	workersWG.Add(workers)
	for i := 0; i < workers; i++ {
		go func() {
			defer workersWG.Done()
			for item := range jobs {
				// Ensure to mark done for this item
				func() {
					defer inFlight.Done()

					// Global stop check (page limit reached)
					if atomic.LoadInt32(&stop) == 1 {
						return
					}

					if job.MaxDepth > 0 && item.depth > job.MaxDepth {
						return
					}

					// host filter
					if job.SameHostOnly && !allowedHosts[strings.ToLower(item.u.Hostname())] {
						return
					}

					key := canonicalPageKey(item.u, job.ConsiderQuery)
					if _, loaded := visited.LoadOrStore(key, true); loaded {
						return
					}

					links, err := fetchProcessAndSaveHTML(item.u, job, client, ua, retries, backoffMs, visitedAssets, assetAllowed, assetWorkers, maxBodySize)
					if err != nil {
						log.Printf("Error processing %s: %v", item.u.String(), err)
						return
					}

					v := pagesSaved.Add(1)
					if job.MaxPages > 0 && int(v) >= job.MaxPages {
						log.Printf("Reached page limit MaxPages=%d", job.MaxPages)
						atomic.StoreInt32(&stop, 1)
						return
					}

					// Enqueue next-level links
					if atomic.LoadInt32(&stop) == 1 {
						return
					}
					for _, ln := range links {
						if ln == nil {
							continue
						}
						if !isHTTP(ln) {
							continue
						}
						if job.SameHostOnly && !allowedHosts[strings.ToLower(ln.Hostname())] {
							continue
						}
						nextDepth := item.depth + 1
						if job.MaxDepth > 0 && nextDepth > job.MaxDepth {
							continue
						}
						// Avoid obvious duplicates in queue (best-effort)
						k := canonicalPageKey(ln, job.ConsiderQuery)
						if _, seen := visited.Load(k); seen {
							continue
						}
						inFlight.Add(1)
						jobs <- crawlQueueItem{u: ln, depth: nextDepth}
					}
				}()
			}
		}()
	}

	// Wait for workers to finish
	workersWG.Wait()
	return nil
}

// Fetch, parse, download assets, rewrite, save

func fetchProcessAndSaveHTML(u *url.URL, job Job, client *http.Client, ua string, retries int, backoffMs int, visitedAssets *sync.Map, assetAllowed map[string]bool, assetWorkers int, maxBodySize int64) ([]*url.URL, error) {
	resp, err := makeRequestWithRetry(client, ua, u, retries, backoffMs)
	if err != nil {
		return nil, fmt.Errorf("HTTP GET: %w", err)
	}
	defer resp.Body.Close()

	finalURL := resp.Request.URL
	contentType := resp.Header.Get("Content-Type")

	ct, _, _ := mime.ParseMediaType(contentType)
	if !strings.Contains(strings.ToLower(ct), "html") {
		// Not HTML, just save raw content as file (binary)
		localPath := urlToLocalPath(finalURL, job.OutputDir, false, job.ConsiderQuery)
		if err := ensureDirForFile(localPath); err != nil {
			return nil, err
		}
		if err := downloadAsset(resp.Body, localPath, maxBodySize); err != nil {
			return nil, fmt.Errorf("save file: %w", err)
		}
		log.Printf("Saved non-HTML resource: %s -> %s", finalURL.String(), localPath)
		return nil, nil
	}

	// HTML
	// Read body for parsing
	body, err := readBody(resp.Body, maxBodySize)
	if err != nil {
		return nil, fmt.Errorf("read HTML body: %w", err)
	}

	doc, err := html.Parse(bytes.NewReader(body))
	if err != nil {
		return nil, fmt.Errorf("parse HTML: %w", err)
	}

	// Collect links and assets
	pageLinks, assetRefs := collectLinksAndAssets(doc, finalURL)

	// Save assets and rewrite URLs
	htmlPath := urlToLocalPath(finalURL, job.OutputDir, true, job.ConsiderQuery)
	htmlDir := filepath.Dir(htmlPath)

	type assetTask struct {
		ref            assetRef
		localAssetPath string
		key            string
	}

	if job.IncludeAssets {
		// Build task list first (filtering by kind and validity)
		tasks := make([]assetTask, 0, len(assetRefs))
		for _, ar := range assetRefs {
			if ar.absURL == nil {
				continue
			}
			kind := ar.kind
			if !isAssetKindAllowed(kind, assetAllowed) {
				continue
			}
			aKey := canonicalAssetKey(ar.absURL)
			localAssetPath := urlToLocalPath(ar.absURL, job.OutputDir, false, false)
			tasks = append(tasks, assetTask{ref: ar, localAssetPath: localAssetPath, key: aKey})
		}

		// Download concurrently with limit
		if assetWorkers <= 0 {
			assetWorkers = 1
		}
		sem := make(chan struct{}, assetWorkers)
		var wg sync.WaitGroup

		for _, t := range tasks {
			// Best-effort dedupe across whole crawl
			if _, loaded := visitedAssets.LoadOrStore(t.key, false); loaded {
				// Already scheduled or done elsewhere
				continue
			}
			wg.Add(1)
			sem <- struct{}{}
			go func(t assetTask) {
				defer wg.Done()
				defer func() { <-sem }()
				// Ensure path
				if err := ensureDirForFile(t.localAssetPath); err != nil {
					visitedAssets.Delete(t.key)
					log.Printf("Asset directory error for %s: %v", t.ref.absURL, err)
					return
				}
				resp, err := makeRequestWithRetry(client, ua, t.ref.absURL, retries, backoffMs)
				if err != nil {
					visitedAssets.Delete(t.key)
					log.Printf("Error fetching asset %s: %v", t.ref.absURL.String(), err)
					return
				}
				defer resp.Body.Close()

				if err := downloadAsset(resp.Body, t.localAssetPath, maxBodySize); err != nil {
					visitedAssets.Delete(t.key)
					log.Printf("Error saving asset %s: %v", t.ref.absURL.String(), err)
					return
				}
				visitedAssets.Store(t.key, true)
				log.Printf("Saved asset: %s -> %s", t.ref.absURL.String(), t.localAssetPath)
			}(t)
		}
		wg.Wait()

		// Rewrite attributes for successfully saved files
		for _, t := range tasks {
			if fileExists(t.localAssetPath) {
				rel, err := filepath.Rel(htmlDir, t.localAssetPath)
				if err == nil {
					setNodeAttrValue(t.ref.node, t.ref.attrIndex, toURLPath(rel), t.ref.isSrcset, t.ref.desc, t.ref.srcsetIndex)
				}
			}
		}
	}

	// Save rewritten HTML
	if err := ensureDirForFile(htmlPath); err != nil {
		return nil, err
	}
	var buf bytes.Buffer
	if err := html.Render(&buf, doc); err != nil {
		return nil, fmt.Errorf("render HTML: %w", err)
	}
	if err := os.WriteFile(htmlPath, buf.Bytes(), 0o644); err != nil {
		return nil, fmt.Errorf("write HTML: %w", err)
	}
	log.Printf("Saved HTML: %s -> %s", finalURL.String(), htmlPath)

	return pageLinks, nil
}

// HTML utilities

func collectLinksAndAssets(doc *html.Node, base *url.URL) (pageLinks []*url.URL, assetRefs []assetRef) {
	pageLinks = make([]*url.URL, 0, 64)
	assetRefs = make([]assetRef, 0, 64)

	// Check for <base href="...">
	var findBase func(*html.Node)
	findBase = func(n *html.Node) {
		if n.Type == html.ElementNode && n.Data == "base" {
			if href := getAttr(n, "href"); href != "" {
				if u := resolveURL(base, href); u != nil {
					base = u
				}
			}
			return
		}
		for c := n.FirstChild; c != nil; c = c.NextSibling {
			findBase(c)
		}
	}
	findBase(doc)

	var walk func(*html.Node)
	walk = func(n *html.Node) {
		if n.Type == html.ElementNode {
			switch strings.ToLower(n.Data) {
			case "a":
				// page links
				if idx, ok := findAttr(n, "href"); ok {
					if u := resolveURL(base, n.Attr[idx].Val); u != nil {
						if isHTTP(u) {
							pageLinks = append(pageLinks, u)
						}
					}
				}
			case "link":
				// CSS, font, etc.
				rel := getAttr(n, "rel")
				asAttr := getAttr(n, "as")
				if idx, ok := findAttr(n, "href"); ok {
					if u := resolveURL(base, n.Attr[idx].Val); u != nil {
						k := assetKindByTag("link", rel, asAttr, u.Path)
						assetRefs = append(assetRefs, assetRef{
							node: n, attrIndex: idx, absURL: u, kind: k,
						})
					}
				}
			case "script":
				if idx, ok := findAttr(n, "src"); ok {
					if u := resolveURL(base, n.Attr[idx].Val); u != nil {
						assetRefs = append(assetRefs, assetRef{
							node: n, attrIndex: idx, absURL: u, kind: assetJS,
						})
					}
				}
			case "img", "source":
				// src
				if idx, ok := findAttr(n, "src"); ok {
					if u := resolveURL(base, n.Attr[idx].Val); u != nil {
						k := assetKindByExt(u.Path)
						if k == assetOTHER {
							k = assetIMG
						}
						assetRefs = append(assetRefs, assetRef{
							node: n, attrIndex: idx, absURL: u, kind: k,
						})
					}
				}
				// srcset
				if idx, ok := findAttr(n, "srcset"); ok {
					entries := parseSrcset(n.Attr[idx].Val)
					for i, e := range entries {
						if u := resolveURL(base, e.url); u != nil {
							k := assetKindByExt(u.Path)
							if k == assetOTHER {
								k = assetIMG
							}
							assetRefs = append(assetRefs, assetRef{
								node: n, attrIndex: idx, absURL: u, kind: k, isSrcset: true, desc: e.descriptor, srcsetIndex: i,
							})
						}
					}
				}
			case "video", "audio":
				// direct src
				if idx, ok := findAttr(n, "src"); ok {
					if u := resolveURL(base, n.Attr[idx].Val); u != nil {
						k := assetKindByExt(u.Path)
						if k == assetOTHER {
							k = assetMEDIA
						}
						assetRefs = append(assetRefs, assetRef{
							node: n, attrIndex: idx, absURL: u, kind: k,
						})
					}
				}
			}
		}
		// recurse
		for c := n.FirstChild; c != nil; c = c.NextSibling {
			walk(c)
		}
	}
	walk(doc)

	return pageLinks, assetRefs
}

func findAttr(n *html.Node, key string) (int, bool) {
	for i := range n.Attr {
		if strings.EqualFold(n.Attr[i].Key, key) {
			return i, true
		}
	}
	return -1, false
}

func getAttr(n *html.Node, key string) string {
	if idx, ok := findAttr(n, key); ok {
		return n.Attr[idx].Val
	}
	return ""
}

// For srcset we parse "url [descriptor]" entries split by comma,
// keep descriptor to be able to rebuild the attribute after rewriting URLs.
type srcsetEntry struct {
	url        string
	descriptor string
}

var spacesRe = regexp.MustCompile(`\s+`)

func parseSrcset(val string) []srcsetEntry {
	out := []srcsetEntry{}
	parts := splitCSVRespectingSpaces(val)
	for _, p := range parts {
		p = strings.TrimSpace(p)
		if p == "" {
			continue
		}
		toks := spacesRe.Split(p, -1)
		if len(toks) == 0 {
			continue
		}
		u := toks[0]
		desc := strings.TrimSpace(strings.TrimPrefix(p, u))
		desc = strings.TrimSpace(desc)
		if strings.HasPrefix(desc, ",") {
			desc = strings.TrimSpace(strings.TrimPrefix(desc, ","))
		}
		out = append(out, srcsetEntry{url: u, descriptor: desc})
	}
	return out
}

func splitCSVRespectingSpaces(s string) []string {
	// srcset uses comma-separated entries; commas inside URLs are rare and not special.
	return strings.Split(s, ",")
}

// Set attribute value. For srcset, rebuild the whole attribute replacing the specific entry.
func setNodeAttrValue(n *html.Node, attrIndex int, newValue string, isSrcset bool, descriptor string, srcsetIndex int) {
	if !isSrcset {
		n.Attr[attrIndex].Val = newValue
		return
	}
	// Rebuild srcset from current value, replacing the specific entry
	old := n.Attr[attrIndex].Val
	entries := parseSrcset(old)

	if srcsetIndex >= 0 && srcsetIndex < len(entries) {
		if descriptor != "" {
			entries[srcsetIndex].descriptor = descriptor
		}
		entries[srcsetIndex].url = newValue
	}

	rebuilt := make([]string, 0, len(entries))
	for _, e := range entries {
		v := strings.TrimSpace(strings.TrimSpace(e.url + " " + e.descriptor))
		rebuilt = append(rebuilt, v)
	}
	n.Attr[attrIndex].Val = strings.Join(rebuilt, ", ")
}

// HTTP utilities

func makeRequestWithRetry(client *http.Client, ua string, u *url.URL, maxRetries int, backoffMs int) (*http.Response, error) {
	var lastErr error
	var resp *http.Response
	var err error
	reqURL := u
	for attempt := 0; attempt <= maxRetries; attempt++ {
		req, rerr := http.NewRequest("GET", reqURL.String(), nil)
		if rerr != nil {
			return nil, rerr
		}
		if ua != "" {
			req.Header.Set("User-Agent", ua)
		}
		resp, err = client.Do(req)
		if err == nil && resp != nil && (resp.StatusCode == http.StatusOK || (resp.StatusCode >= 200 && resp.StatusCode < 300)) {
			return resp, nil
		}
		if resp != nil {
			_ = resp.Body.Close()
		}
		if err != nil {
			lastErr = err
		} else if resp != nil {
			lastErr = fmt.Errorf("http status %d", resp.StatusCode)
			// retry on 5xx or 429
			if resp.StatusCode < 500 && resp.StatusCode != 429 {
				break
			}
		}

		// Calculate backoff with jitter
		sleepDuration := time.Duration(backoffMs*(attempt+1)) * time.Millisecond
		// Add up to 20% jitter
		jitter := time.Duration(rand.Int63n(int64(sleepDuration) / 5))
		sleepDuration += jitter

		// Respect Retry-After header if present
		if resp != nil && resp.StatusCode == 429 {
			if retryAfter := resp.Header.Get("Retry-After"); retryAfter != "" {
				if seconds, err := strconv.Atoi(retryAfter); err == nil {
					sleepDuration = time.Duration(seconds) * time.Second
				} else if t, err := time.Parse(time.RFC1123, retryAfter); err == nil {
					sleepDuration = time.Until(t)
				}
			}
		}

		time.Sleep(sleepDuration)
	}
	if lastErr == nil {
		lastErr = fmt.Errorf("unknown HTTP error for %s", u.String())
	}
	return nil, lastErr
}

func readBody(r io.Reader, maxBodySize int64) ([]byte, error) {
	if maxBodySize > 0 {
		r = io.LimitReader(r, maxBodySize+1)
	}
	b, err := io.ReadAll(r)
	if err != nil {
		return nil, err
	}
	if maxBodySize > 0 && int64(len(b)) > maxBodySize {
		return nil, fmt.Errorf("body size exceeds limit %d", maxBodySize)
	}
	return b, nil
}

func downloadAsset(r io.Reader, path string, maxBodySize int64) error {
	f, err := os.Create(path)
	if err != nil {
		return err
	}
	defer f.Close()

	if maxBodySize > 0 {
		r = io.LimitReader(r, maxBodySize+1)
	}

	n, err := io.Copy(f, r)
	if err != nil {
		return err
	}
	if maxBodySize > 0 && n > maxBodySize {
		os.Remove(path)
		return fmt.Errorf("asset size exceeds limit %d", maxBodySize)
	}
	return nil
}

// Path and URL mapping

func urlToLocalPath(u *url.URL, outDir string, isHTML bool, considerQuery bool) string {
	hostPart := sanitizePathPart(u.Hostname())
	p := u.Path
	if p == "" {
		p = "/"
	}
	p = path.Clean(p)

	// handle directories and extensions
	base := path.Base(p)
	if strings.HasSuffix(p, "/") || base == "." || base == "/" {
		if isHTML {
			p = path.Join(p, "index.html")
		} else {
			p = path.Join(p, "index")
		}
	} else {
		if isHTML {
			// if no extension, add .html
			if ext := path.Ext(base); ext == "" {
				p = p + ".html"
			}
		}
	}

	// include query hash for non-HTML assets to avoid collisions, or if considerQuery is true
	if (!isHTML || considerQuery) && u.RawQuery != "" {
		dir := path.Dir(p)
		base := path.Base(p)
		ext := path.Ext(base)
		name := strings.TrimSuffix(base, ext)
		h := sha1.Sum([]byte(u.RawQuery))
		s := hex.EncodeToString(h[:])[:8]
		p = path.Join(dir, fmt.Sprintf("%s-%s%s", name, s, ext))
	}

	// strip leading slash to avoid absolute path joining
	p = strings.TrimPrefix(p, "/")

	full := filepath.Join(outDir, hostPart, filepath.FromSlash(p))
	return full
}

func sanitizePathPart(s string) string {
	// Handle IPv6 literals by stripping brackets
	s = strings.ReplaceAll(s, "[", "")
	s = strings.ReplaceAll(s, "]", "")

	// Replace unsafe characters with underscore
	// Allow a-z, A-Z, 0-9, ., _, -
	safe := func(r rune) rune {
		switch {
		case r >= 'a' && r <= 'z':
			return r
		case r >= 'A' && r <= 'Z':
			return r
		case r >= '0' && r <= '9':
			return r
		case r == '.' || r == '_' || r == '-':
			return r
		default:
			return '_'
		}
	}
	s = strings.Map(safe, s)

	// Prevent directory traversal
	s = strings.ReplaceAll(s, "..", "")
	s = strings.TrimSpace(s)
	if s == "" {
		return "unknown"
	}
	return s
}

func toURLPath(p string) string {
	// convert OS path to URL path with '/'
	return filepath.ToSlash(p)
}

// Helpers

func parseURL(s string) *url.URL {
	u, err := url.Parse(s)
	if err != nil {
		return nil
	}
	if u.Scheme == "" {
		u.Scheme = "https"
	}
	return u
}

func isHTTP(u *url.URL) bool {
	if u == nil {
		return false
	}
	switch strings.ToLower(u.Scheme) {
	case "http", "https":
		return true
	default:
		return false
	}
}

func resolveURL(base *url.URL, ref string) *url.URL {
	ref = strings.TrimSpace(ref)
	if ref == "" {
		return nil
	}
	// ignore data URLs
	if strings.HasPrefix(ref, "data:") {
		return nil
	}
	u, err := url.Parse(ref)
	if err != nil {
		return nil
	}
	if base == nil {
		return u
	}
	return base.ResolveReference(u)
}

func canonicalPageKey(u *url.URL, considerQuery bool) string {
	// ignore fragment and query for page uniqueness unless considerQuery is true
	key := strings.ToLower(u.Scheme) + "://" + strings.ToLower(u.Host) + u.EscapedPath()
	if considerQuery && u.RawQuery != "" {
		key += "?" + u.RawQuery
	}
	return key
}

func canonicalAssetKey(u *url.URL) string {
	fragless := *u
	fragless.Fragment = ""
	return fragless.String()
}

func ensureDirForFile(filePath string) error {
	dir := filepath.Dir(filePath)
	return os.MkdirAll(dir, 0o755)
}

func fileExists(p string) bool {
	st, err := os.Stat(p)
	return err == nil && !st.IsDir()
}

func isAssetKindAllowed(k assetRefKind, allowed map[string]bool) bool {
	if len(allowed) == 0 {
		return false
	}
	switch k {
	case assetCSS:
		return allowed["css"]
	case assetJS:
		return allowed["js"]
	case assetIMG:
		return allowed["img"]
	case assetFONT:
		return allowed["font"]
	case assetMEDIA:
		return allowed["media"]
	case assetOTHER:
		return allowed["other"]
	default:
		return false
	}
}

func assetKindByTag(tag string, rel string, asAttr string, p string) assetRefKind {
	relLower := strings.ToLower(rel)
	asLower := strings.ToLower(asAttr)
	extKind := assetKindByExt(p)
	if strings.Contains(relLower, "stylesheet") {
		return assetCSS
	}
	if asLower == "font" {
		return assetFONT
	}
	if extKind != assetOTHER {
		return extKind
	}
	return assetOTHER
}

func assetKindByExt(p string) assetRefKind {
	ext := strings.ToLower(path.Ext(p))
	switch ext {
	case ".css":
		return assetCSS
	case ".js", ".mjs":
		return assetJS
	case ".png", ".jpg", ".jpeg", ".gif", ".webp", ".svg", ".avif":
		return assetIMG
	case ".woff", ".woff2", ".ttf", ".otf", ".eot":
		return assetFONT
	case ".mp4", ".webm", ".mp3", ".ogg", ".wav", ".mov", ".m4a":
		return assetMEDIA
	default:
		return assetOTHER
	}
}
