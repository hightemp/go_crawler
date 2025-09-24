# go-crawler

Simple CLI utility to download web pages or entire sites to a local folder based on a YAML configuration.

## Configuration

See the example in [`config.example.yaml`](config.example.yaml). Structure:

```yaml
http:
  timeout_sec: 20           # HTTP client timeout, seconds
  user_agent: "go_crawler/0.1 (+https://example.local)"
  max_retries: 2            # number of retries on errors
  retry_backoff_ms: 300     # backoff between retries (increasing per attempt)

# Set of crawl jobs. Each job can be enabled/disabled, has a type and a list of URLs
items:
  # 1) Download a single page (type: page)
  - enabled: true
    type: page                # page | pages | site
    urls:
      - "https://example.com/"
    output_dir: "out/page_example"
    include_assets: true
    asset_types: ["css", "js", "img"]  # css, js, img, font, media
    same_host_only: true
    max_depth: 0    # not used for page
    max_pages: 0    # not used for page

  # 2) Crawl multiple pages by following links on the same host (type: pages)
  - enabled: false
    type: pages
    urls:
      - "https://example.com/"
    output_dir: "out/pages_example"
    include_assets: true
    asset_types: ["css", "js", "img", "font"]
    same_host_only: true
    max_depth: 2     # crawl depth
    max_pages: 50    # limit number of pages

  # 3) Save a site (similar to pages, typically with same_host_only enabled)
  - enabled: false
    type: site
    urls:
      - "https://example.com/"
    output_dir: "out/site_example"
    include_assets: true
    asset_types: ["css", "js", "img", "font", "media"]
    same_host_only: true
    max_depth: 3
    max_pages: 200
```

Field reference:
- http
  - timeout_sec: integer seconds (default 20).
  - user_agent: string for User-Agent header.
  - max_retries: non-negative integer (default 0).
  - retry_backoff_ms: positive integer milliseconds (default 250).
- items[] (job)
  - enabled: bool to toggle job.
  - type: "page" | "pages" | "site".
  - urls: list of absolute URLs to start from.
  - output_dir: target directory for saving files.
  - include_assets: download and rewrite assets in HTML.
  - asset_types: subset of ["css","js","img","font","media"]; used only if include_assets is true. Default ["css","js","img"].
  - same_host_only: restrict crawl to hosts derived from the seed URLs (recommended for "site").
  - max_depth: integer depth limit for BFS (0 means unlimited).
  - max_pages: integer total page limit (0 means unlimited).

## Output mapping

- Output files are stored under: `<output_dir>/<host>/<path...>`.
- Directory-like URLs (ending with `/`) become `index.html` when downloading HTML.
- URLs without extension become `<name>.html` for HTML pages.
- Non-HTML assets keep their extension; if query string is present, an 8-char SHA1 of the query is appended before the extension: `name-xxxxxxxx.ext`.

Examples:
- https://example.com/ → `out/.../example.com/index.html`
- https://example.com/about → `out/.../example.com/about.html`
- https://static.example.com/main.css?v=123 → `out/.../static.example.com/main-<hash>.css`

## Crawling and rewriting

- BFS starting from the provided URLs (for "pages"/"site" types).
- Only http/https links considered; fragments ignored for uniqueness.
- Asset references rewritten inside HTML:
  - Attributes: href (link), src (script/img/source/video/audio), srcset (img/source).
  - Rewritten to relative paths from the saved HTML location.
- Data URLs are ignored.
- Host restriction via same_host_only.

## License

MIT