package main

import (
	"bytes"
	"crypto/sha1"
	"encoding/hex"
	"fmt"
	"io"
	"log"
	"mime"
	"net/http"
	"net/url"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"golang.org/x/net/html"
)

// ---------- job runner ----------

func runJob(job Job, client *http.Client, ua string, retries int, backoffMs int) error {
	if len(job.Urls) == 0 {
		return fmt.Errorf("job без urls")
	}
	if strings.TrimSpace(job.OutputDir) == "" {
		return fmt.Errorf("job без output_dir")
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
		return fmt.Errorf("создание каталога вывода: %w", err)
	}

	// allowed hosts
	allowedHosts := map[string]bool{}
	for _, s := range job.Urls {
		if u := parseURL(s); u != nil {
			allowedHosts[strings.ToLower(u.Host)] = true
		}
	}

	switch job.Type {
	case "page":
		for _, s := range job.Urls {
			u := parseURL(s)
			if u == nil {
				log.Printf("Пропуск некорректного URL: %s", s)
				continue
			}
			if err := crawlSinglePage(u, job, client, ua, retries, backoffMs, assetAllowed); err != nil {
				log.Printf("Ошибка скачивания страницы %s: %v", u.String(), err)
			}
		}
	case "pages", "site":
		if err := bfsCrawl(job, client, ua, retries, backoffMs, assetAllowed, allowedHosts); err != nil {
			return err
		}
	default:
		return fmt.Errorf("неизвестный type: %s", job.Type)
	}
	return nil
}

// ---------- crawling ----------

func crawlSinglePage(u *url.URL, job Job, client *http.Client, ua string, retries int, backoffMs int, assetAllowed map[string]bool) error {
	visitedAssets := make(map[string]bool)
	_, err := fetchProcessAndSaveHTML(u, job, client, ua, retries, backoffMs, visitedAssets, assetAllowed)
	return err
}

func bfsCrawl(job Job, client *http.Client, ua string, retries int, backoffMs int, assetAllowed map[string]bool, allowedHosts map[string]bool) error {
	queue := make([]crawlQueueItem, 0, 128)
	visited := map[string]bool{}
	visitedAssets := map[string]bool{}

	for _, s := range job.Urls {
		if u := parseURL(s); u != nil {
			queue = append(queue, crawlQueueItem{u: u, depth: 0})
		}
	}

	pagesSaved := 0
	for len(queue) > 0 {
		item := queue[0]
		queue = queue[1:]

		if job.MaxDepth > 0 && item.depth > job.MaxDepth {
			continue
		}
		key := canonicalPageKey(item.u)
		if visited[key] {
			continue
		}
		visited[key] = true

		// host filter
		if job.SameHostOnly && !allowedHosts[strings.ToLower(item.u.Host)] {
			continue
		}

		links, err := fetchProcessAndSaveHTML(item.u, job, client, ua, retries, backoffMs, visitedAssets, assetAllowed)
		if err != nil {
			log.Printf("Ошибка обработки %s: %v", item.u.String(), err)
			continue
		}
		pagesSaved++
		if job.MaxPages > 0 && pagesSaved >= job.MaxPages {
			log.Printf("Достигнут лимит страниц MaxPages=%d", job.MaxPages)
			break
		}

		// Enqueue next-level links
		for _, ln := range links {
			if ln == nil {
				continue
			}
			if !isHTTP(ln) {
				continue
			}
			if job.SameHostOnly && !allowedHosts[strings.ToLower(ln.Host)] {
				continue
			}
			k := canonicalPageKey(ln)
			if !visited[k] {
				queue = append(queue, crawlQueueItem{u: ln, depth: item.depth + 1})
			}
		}
	}

	return nil
}

// fetch, parse, download assets, rewrite, save
func fetchProcessAndSaveHTML(u *url.URL, job Job, client *http.Client, ua string, retries int, backoffMs int, visitedAssets map[string]bool, assetAllowed map[string]bool) ([]*url.URL, error) {
	body, contentType, finalURL, err := httpGetWithRetry(client, ua, u, retries, backoffMs)
	if err != nil {
		return nil, fmt.Errorf("HTTP GET: %w", err)
	}

	ct, _, _ := mime.ParseMediaType(contentType)
	if !strings.Contains(strings.ToLower(ct), "html") {
		// Not HTML, just save raw content as file (binary)
		localPath := urlToLocalPath(finalURL, job.OutputDir, false)
		if err := ensureDirForFile(localPath); err != nil {
			return nil, err
		}
		if err := os.WriteFile(localPath, body, 0o644); err != nil {
			return nil, fmt.Errorf("сохранение файла: %w", err)
		}
		log.Printf("Сохранён не-HTML ресурс: %s -> %s", finalURL.String(), localPath)
		return nil, nil
	}

	// HTML
	doc, err := html.Parse(bytes.NewReader(body))
	if err != nil {
		return nil, fmt.Errorf("парсинг HTML: %w", err)
	}

	// Collect links and assets
	pageLinks, assetRefs := collectLinksAndAssets(doc, finalURL)

	// Save assets and rewrite URLs
	htmlPath := urlToLocalPath(finalURL, job.OutputDir, true)
	htmlDir := filepath.Dir(htmlPath)

	if job.IncludeAssets {
		for _, ar := range assetRefs {
			if ar.absURL == nil {
				continue
			}
			kind := ar.kind
			if !isAssetKindAllowed(kind, assetAllowed) {
				continue
			}
			// asset download (dedupe)
			aKey := canonicalAssetKey(ar.absURL)
			localAssetPath := urlToLocalPath(ar.absURL, job.OutputDir, false)

			if !visitedAssets[aKey] {
				if err := ensureDirForFile(localAssetPath); err != nil {
					log.Printf("Ошибка каталога для ассета %s: %v", ar.absURL, err)
				} else {
					content, _, _, err := httpGetWithRetry(client, ua, ar.absURL, retries, backoffMs)
					if err != nil {
						log.Printf("Ошибка скачивания ассета %s: %v", ar.absURL.String(), err)
					} else {
						if err := os.WriteFile(localAssetPath, content, 0o644); err != nil {
							log.Printf("Ошибка сохранения ассета %s: %v", ar.absURL.String(), err)
						} else {
							visitedAssets[aKey] = true
							log.Printf("Сохранён ассет: %s -> %s", ar.absURL.String(), localAssetPath)
						}
					}
				}
			}

			// rewrite attribute to relative path even if failure? Only if file exists
			if fileExists(localAssetPath) {
				rel, err := filepath.Rel(htmlDir, localAssetPath)
				if err == nil {
					setNodeAttrValue(ar.node, ar.attrIndex, toURLPath(rel), ar.isSrcset, ar.desc)
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
		return nil, fmt.Errorf("рендер HTML: %w", err)
	}
	if err := os.WriteFile(htmlPath, buf.Bytes(), 0o644); err != nil {
		return nil, fmt.Errorf("сохранение HTML: %w", err)
	}
	log.Printf("Сохранён HTML: %s -> %s", finalURL.String(), htmlPath)

	return pageLinks, nil
}

// ---------- HTML utilities ----------

func collectLinksAndAssets(doc *html.Node, base *url.URL) (pageLinks []*url.URL, assetRefs []assetRef) {
	pageLinks = make([]*url.URL, 0, 64)
	assetRefs = make([]assetRef, 0, 64)

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
						if k != assetOTHER {
							assetRefs = append(assetRefs, assetRef{
								node: n, attrIndex: idx, absURL: u, kind: k,
							})
						}
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
					for _, e := range entries {
						if u := resolveURL(base, e.url); u != nil {
							k := assetKindByExt(u.Path)
							if k == assetOTHER {
								k = assetIMG
							}
							assetRefs = append(assetRefs, assetRef{
								node: n, attrIndex: idx, absURL: u, kind: k, isSrcset: true, desc: e.descriptor,
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

// For srcset, we store all entries for later rewrite by grouping. To simplify,
// we rewrite attribute value on each assetRef occurrence by rebuilding full srcset in setNodeAttrValue.
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
	// So a simple split by comma is acceptable.
	parts := strings.Split(s, ",")
	return parts
}

// set attribute value. For srcset, we need to rebuild combined value. We will append/replace progressively.
// To ensure correctness, for srcset we rebuild from existing value and replace URL matches.
func setNodeAttrValue(n *html.Node, attrIndex int, newValue string, isSrcset bool, descriptor string) {
	if !isSrcset {
		n.Attr[attrIndex].Val = newValue
		return
	}
	// Rebuild srcset from current value, replacing the first matching URL occurrence that corresponds to this node call.
	// Simpler approach: if descriptor present, replace first entry with same descriptor; otherwise replace first entry.
	old := n.Attr[attrIndex].Val
	entries := parseSrcset(old)
	rebuilt := make([]string, 0, len(entries))
	replaced := false
	for _, e := range entries {
		if !replaced {
			if descriptor != "" {
				rebuilt = append(rebuilt, strings.TrimSpace(strings.TrimSpace(newValue+" "+descriptor)))
				replaced = true
				continue
			}
			rebuilt = append(rebuilt, newValue)
			replaced = true
			continue
		}
		v := strings.TrimSpace(strings.TrimSpace(e.url + " " + e.descriptor))
		rebuilt = append(rebuilt, v)
	}
	// If nothing replaced (shouldn't happen), append
	if !replaced {
		if descriptor != "" {
			rebuilt = append(rebuilt, strings.TrimSpace(strings.TrimSpace(newValue+" "+descriptor)))
		} else {
			rebuilt = append(rebuilt, newValue)
		}
	}
	n.Attr[attrIndex].Val = strings.Join(rebuilt, ", ")
}

// ---------- HTTP utilities ----------

func httpGetWithRetry(client *http.Client, ua string, u *url.URL, maxRetries int, backoffMs int) ([]byte, string, *url.URL, error) {
	var lastErr error
	var resp *http.Response
	var err error
	reqURL := u
	for attempt := 0; attempt <= maxRetries; attempt++ {
		req, rerr := http.NewRequest("GET", reqURL.String(), nil)
		if rerr != nil {
			return nil, "", reqURL, rerr
		}
		if ua != "" {
			req.Header.Set("User-Agent", ua)
		}
		resp, err = client.Do(req)
		if err == nil && resp != nil && (resp.StatusCode == http.StatusOK || (resp.StatusCode >= 200 && resp.StatusCode < 300)) {
			defer resp.Body.Close()
			b, rerr := io.ReadAll(resp.Body)
			if rerr != nil {
				return nil, "", reqURL, rerr
			}
			finalURL := reqURL
			if resp.Request != nil && resp.Request.URL != nil {
				finalURL = resp.Request.URL
			}
			ct := strings.TrimSpace(resp.Header.Get("Content-Type"))
			return b, ct, finalURL, nil
		}
		if resp != nil {
			_ = resp.Body.Close()
		}
		if err != nil {
			lastErr = err
		} else if resp != nil {
			lastErr = fmt.Errorf("http status %d", resp.StatusCode)
			// retry on 5xx
			if resp.StatusCode < 500 {
				break
			}
		}
		time.Sleep(time.Duration(backoffMs*(attempt+1)) * time.Millisecond)
	}
	if lastErr == nil {
		lastErr = fmt.Errorf("неизвестная ошибка HTTP при запросе %s", u.String())
	}
	return nil, "", u, lastErr
}

// ---------- Path and URL mapping ----------

func urlToLocalPath(u *url.URL, outDir string, isHTML bool) string {
	hostPart := sanitizePathPart(u.Host)
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
		} else {
			// keep extension; if no extension, keep filename as is
		}
	}

	// include query hash for non-HTML assets to avoid collisions
	if !isHTML && u.RawQuery != "" {
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
	s = strings.ReplaceAll(s, "..", "")
	s = strings.TrimSpace(s)
	return s
}

func toURLPath(p string) string {
	// convert OS path to URL path with '/'
	return filepath.ToSlash(p)
}

// ---------- Helpers ----------

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
	// data URLs are ignored
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

func canonicalPageKey(u *url.URL) string {
	// ignore fragment and query for page uniqueness
	return strings.ToLower(u.Scheme) + "://" + strings.ToLower(u.Host) + u.EscapedPath()
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
		return false
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
