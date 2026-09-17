package webpages

import (
	"bufio"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/xml"
	"errors"
	"fmt"
	"net/url"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"sync"

	"golang.org/x/net/html"
)

const (
	defaultCrawlPages = 1000
	maxCrawlPages     = 10000
	defaultCrawlDepth = 6
)

// Crawl discovers pages by following links from seed pages and sitemaps.
// Only URLs under an include prefix are fetched; linked documents (PDFs and
// CMS assets) under a documents prefix are kept without being followed.
type Crawl struct {
	Start     []string `yaml:"start" json:"start,omitempty"`
	Sitemaps  []string `yaml:"sitemaps" json:"sitemaps,omitempty"`
	Include   []string `yaml:"include" json:"include"`
	Exclude   []string `yaml:"exclude" json:"exclude,omitempty"`
	Documents []string `yaml:"documents" json:"documents,omitempty"`
	MaxPages  int      `yaml:"max_pages" json:"max_pages,omitempty"`
	MaxDepth  int      `yaml:"max_depth" json:"max_depth,omitempty"`
}

func (c *Crawl) validate(index int) error {
	if len(c.Start) == 0 && len(c.Sitemaps) == 0 {
		return fmt.Errorf("crawl %d: start pages or sitemaps are required", index)
	}
	if len(c.Include) == 0 {
		return fmt.Errorf("crawl %d: include prefixes are required", index)
	}
	for _, group := range [][]string{c.Start, c.Sitemaps, c.Include, c.Documents} {
		for _, raw := range group {
			parsed, err := url.Parse(raw)
			if err != nil || (parsed.Scheme != "http" && parsed.Scheme != "https") || parsed.Host == "" {
				return fmt.Errorf("crawl %d: invalid URL %q", index, raw)
			}
		}
	}
	for _, start := range c.Start {
		if !hasPrefix(normalizeURL(start), c.Include) {
			return fmt.Errorf("crawl %d: start page %q is outside the include prefixes", index, start)
		}
	}
	if c.MaxPages <= 0 {
		c.MaxPages = defaultCrawlPages
	}
	c.MaxPages = min(c.MaxPages, maxCrawlPages)
	if c.MaxDepth <= 0 {
		c.MaxDepth = defaultCrawlDepth
	}
	return nil
}

func hasPrefix(target string, prefixes []string) bool {
	for _, prefix := range prefixes {
		if strings.HasPrefix(target, prefix) {
			return true
		}
	}
	return false
}

func (c *Crawl) excluded(target string) bool {
	for _, pattern := range c.Exclude {
		if strings.Contains(target, pattern) {
			return true
		}
	}
	return false
}

// normalizeURL drops fragments and default ports so one page has one key.
func normalizeURL(raw string) string {
	parsed, err := url.Parse(strings.TrimSpace(raw))
	if err != nil {
		return raw
	}
	parsed.Fragment, parsed.RawFragment = "", ""
	parsed.Host = strings.ToLower(parsed.Host)
	parsed.Host = strings.TrimSuffix(strings.TrimSuffix(parsed.Host, ":443"), ":80")
	if parsed.Path == "" {
		parsed.Path = "/"
	}
	return parsed.String()
}

// urlKey identifies a page for deduplication: sites serve "/a" and "/a/"
// as the same page.
func urlKey(raw string) string {
	key := normalizeURL(raw)
	if parsed, err := url.Parse(key); err == nil && parsed.Path != "/" && strings.HasSuffix(parsed.Path, "/") {
		parsed.Path = strings.TrimSuffix(parsed.Path, "/")
		parsed.RawPath = ""
		return parsed.String()
	}
	return key
}

var slugCleaner = regexp.MustCompile(`[^a-z0-9]+`)

// slugForURL derives a stable dataset-unique slug from a URL.
func slugForURL(raw string, used map[string]string) string {
	parsed, _ := url.Parse(raw)
	host := strings.TrimPrefix(strings.ToLower(parsed.Hostname()), "www.")
	host = strings.Split(host, ".")[0]
	text := host + "-" + parsed.Path
	if parsed.RawQuery != "" {
		text += "-" + parsed.RawQuery
	}
	text = strings.NewReplacer("æ", "ae", "ø", "oe", "å", "aa", "é", "e").Replace(strings.ToLower(text))
	slug := strings.Trim(slugCleaner.ReplaceAllString(text, "-"), "-")
	if len(slug) > 100 {
		slug = strings.Trim(slug[:100], "-")
	}
	if owner, taken := used[slug]; taken && owner != raw {
		sum := sha256.Sum256([]byte(raw))
		slug = slug + "-" + hex.EncodeToString(sum[:3])
	}
	used[slug] = raw
	return slug
}

// crawlResult is one discovered page. Fetched HTML and PDF pages carry
// their stored entry so the fetch phase does not request them again. Static
// results are statically listed pages the crawl reached and fetched.
type crawlResult struct {
	page   Page
	entry  *pageEntry
	static bool
}

// genericLinkText is anchor text that says nothing about the target.
var genericLinkText = regexp.MustCompile(`(?i)^(download|hent|hent her|her|klik her|læs mere|se mere|pdf|link|åbn)\.?$`)

// discover runs every crawl of the configuration and returns new pages in a
// stable order. Pages already listed statically are skipped.
func (p *WebPages) discover(ctx context.Context, config Config, rawDir string, known map[string]Page, used map[string]string, cached func(Page) (pageEntry, bool), progress func(int, string)) ([]crawlResult, error) {
	if progress == nil {
		progress = func(int, string) {}
	}
	var results []crawlResult
	state := &crawlState{seen: map[string]bool{}, contents: map[string]bool{}, known: known, used: used}
	for index := range config.Crawl {
		found, err := p.crawl(ctx, config, &config.Crawl[index], rawDir, state, cached, func(count int, message string) { progress(len(results)+count, message) })
		if err != nil {
			return nil, err
		}
		results = append(results, found...)
	}
	return results, nil
}

// crawlState is shared by all crawls of one release so a URL or a page body
// is kept once, whichever site it was found on first.
type crawlState struct {
	seen     map[string]bool
	contents map[string]bool
	known    map[string]Page
	used     map[string]string
}

func (p *WebPages) crawl(ctx context.Context, config Config, crawl *Crawl, rawDir string, state *crawlState, cached func(Page) (pageEntry, bool), progress func(int, string)) ([]crawlResult, error) {
	seen, used := state.seen, state.used
	robots := p.robotsRules(ctx, config, append(append([]string{}, crawl.Start...), crawl.Include...))
	allowed := func(target string) bool {
		parsed, err := url.Parse(target)
		if err != nil {
			return false
		}
		for _, rule := range robots[parsed.Scheme+"://"+parsed.Host] {
			if rule.MatchString(parsed.RequestURI()) {
				return false
			}
		}
		return true
	}
	var results []crawlResult
	var mu sync.Mutex
	type item struct {
		url   string
		title string
		depth int
		doc   bool
	}
	var frontier []item
	enqueue := func(raw, title string, depth int) {
		target := normalizeURL(raw)
		key := urlKey(target)
		if seen[key] || crawl.excluded(target) || !allowed(target) {
			return
		}
		if static, ok := state.known[key]; ok && static.Type == TypePDF {
			return
		}
		isDoc := hasPrefix(target, crawl.Documents) || strings.EqualFold(path.Ext(pathOf(target)), ".pdf") && hasPrefix(target, crawl.Include)
		if !isDoc && !hasPrefix(target, crawl.Include) {
			return
		}
		seen[key] = true
		if genericLinkText.MatchString(strings.TrimSpace(title)) || len(title) > 200 {
			title = ""
		}
		frontier = append(frontier, item{url: target, title: title, depth: depth, doc: isDoc})
	}
	for _, start := range crawl.Start {
		enqueue(start, "", 0)
	}
	for _, sitemap := range crawl.Sitemaps {
		urls, err := p.sitemapURLs(ctx, config, sitemap, 0)
		if err != nil && len(crawl.Start) == 0 {
			return nil, err
		}
		sort.Strings(urls)
		for _, target := range urls {
			enqueue(target, "", 1)
		}
	}
	pages := 0
	for len(frontier) > 0 && pages < crawl.MaxPages {
		batch := frontier
		frontier = nil
		if room := crawl.MaxPages - pages; len(batch) > room {
			batch = batch[:room]
		}
		type outcome struct {
			result crawlResult
			links  []link
			digest string
		}
		outcomes := make([]outcome, len(batch))
		fetchedPages := 0
		tasks := make(chan int)
		var wg sync.WaitGroup
		for range min(config.Concurrency, len(batch)) {
			wg.Add(1)
			go func() {
				defer wg.Done()
				for index := range tasks {
					current := batch[index]
					page, static := state.known[urlKey(current.url)]
					if !static {
						mu.Lock()
						slug := slugForURL(current.url, used)
						mu.Unlock()
						page = Page{Slug: slug, Title: current.title, URL: current.url, Type: TypeHTML}
						if current.doc {
							page.Type = TypePDF
						}
					}
					entry, reused := cached(page)
					if reused && entry.Type != page.Type && static {
						reused = false
					}
					var body []byte
					if reused {
						body, _ = readRaw(rawDir, entry)
					} else {
						entry, body = p.fetchPageBody(ctx, config, page, rawDir)
						if ctx.Err() == nil {
							_ = p.sleep(ctx, config.delay())
						}
					}
					if entry.Status != statusOK {
						outcomes[index] = outcome{result: crawlResult{page: page, entry: &entry, static: static}}
						continue
					}
					if !static {
						page.Type = entry.Type
						if page.Title == "" {
							page.Title = entry.Title
						}
					}
					var links []link
					if entry.Type == TypeHTML && !current.doc && current.depth < crawl.MaxDepth {
						links = pageLinks(body, firstNonEmpty(entry.ResolvedURL, page.URL))
					}
					outcomes[index] = outcome{result: crawlResult{page: page, entry: &entry, static: static}, links: links, digest: contentDigest(body, entry)}
					mu.Lock()
					fetchedPages++
					progress(len(results)+fetchedPages, fmt.Sprintf("crawling %s (%d pages checked)", crawl.Include[0], len(results)+fetchedPages))
					mu.Unlock()
				}
			}()
		}
	send:
		for index := range batch {
			select {
			case tasks <- index:
			case <-ctx.Done():
				break send
			}
		}
		close(tasks)
		wg.Wait()
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		for index, current := range batch {
			result := outcomes[index]
			// Failed crawl fetches are dropped: an unreachable URL found by
			// crawling is not a curated page worth reporting.
			if result.result.entry != nil && result.result.entry.Status != statusOK && !result.result.static {
				continue
			}
			// Mirrored copies of a page on other sites are dropped; the
			// first copy, in crawl order, is kept.
			if result.digest != "" {
				if state.contents[result.digest] && !result.result.static {
					_ = os.Remove(filepath.Join(rawDir, result.result.entry.File))
					continue
				}
				state.contents[result.digest] = true
			}
			results = append(results, result.result)
			pages++
			for _, found := range result.links {
				enqueue(found.url, found.text, current.depth+1)
			}
		}
		progress(len(results), fmt.Sprintf("crawling %s (%d pages found)", crawl.Include[0], len(results)))
	}
	return results, nil
}

type link struct {
	url  string
	text string
}

// pageLinks returns the absolute http(s) links of an HTML page with their
// anchor text.
func pageLinks(source []byte, pageURL string) []link {
	document, err := html.Parse(bytes.NewReader(source))
	if err != nil {
		return nil
	}
	base, err := url.Parse(pageURL)
	if err != nil {
		return nil
	}
	var links []link
	var walk func(*html.Node)
	walk = func(node *html.Node) {
		if node.Type == html.ElementNode && node.Data == "base" {
			for _, attribute := range node.Attr {
				if attribute.Key == "href" {
					if parsed, err := base.Parse(attribute.Val); err == nil {
						base = parsed
					}
				}
			}
		}
		if node.Type == html.ElementNode && node.Data == "a" {
			for _, attribute := range node.Attr {
				if attribute.Key != "href" {
					continue
				}
				reference, err := url.Parse(strings.TrimSpace(attribute.Val))
				if err != nil {
					continue
				}
				resolved := base.ResolveReference(reference)
				if resolved.Scheme == "http" || resolved.Scheme == "https" {
					links = append(links, link{url: resolved.String(), text: strings.Join(strings.Fields(nodeText(node)), " ")})
				}
			}
		}
		for child := node.FirstChild; child != nil; child = child.NextSibling {
			walk(child)
		}
	}
	walk(document)
	return links
}

func nodeText(node *html.Node) string {
	var builder strings.Builder
	var walk func(*html.Node)
	walk = func(current *html.Node) {
		if current.Type == html.TextNode {
			builder.WriteString(current.Data)
			builder.WriteByte(' ')
		}
		for child := current.FirstChild; child != nil; child = child.NextSibling {
			walk(child)
		}
	}
	walk(node)
	return builder.String()
}

type sitemapDocument struct {
	URLs     []string `xml:"url>loc"`
	Sitemaps []string `xml:"sitemap>loc"`
}

// sitemapURLs reads a sitemap or sitemap index, following nested indexes.
func (p *WebPages) sitemapURLs(ctx context.Context, config Config, target string, depth int) ([]string, error) {
	if depth > 3 {
		return nil, errors.New("sitemap index nesting is too deep")
	}
	response, err := p.get(ctx, config, target)
	if err != nil {
		return nil, err
	}
	var document sitemapDocument
	if err := xml.Unmarshal(response.body, &document); err != nil {
		return nil, fmt.Errorf("parse sitemap %s: %w", target, err)
	}
	urls := make([]string, 0, len(document.URLs))
	for _, loc := range document.URLs {
		urls = append(urls, strings.TrimSpace(loc))
	}
	for _, nested := range document.Sitemaps {
		more, err := p.sitemapURLs(ctx, config, strings.TrimSpace(nested), depth+1)
		if err != nil {
			return nil, err
		}
		urls = append(urls, more...)
	}
	return urls, nil
}

// robotsRules returns the Disallow prefixes that apply to every agent, per
// origin. An unreadable robots.txt imposes no rules.
func (p *WebPages) robotsRules(ctx context.Context, config Config, targets []string) map[string][]*regexp.Regexp {
	rules := map[string][]*regexp.Regexp{}
	for _, target := range targets {
		parsed, err := url.Parse(target)
		if err != nil {
			continue
		}
		origin := parsed.Scheme + "://" + parsed.Host
		if _, done := rules[origin]; done {
			continue
		}
		rules[origin] = nil
		response, err := p.get(ctx, config, origin+"/robots.txt")
		if err != nil {
			continue
		}
		rules[origin] = parseRobots(response.body)
	}
	return rules
}

// parseRobots returns matchers for the Disallow rules of the "*" group,
// supporting the "*" wildcard and "$" end anchor.
func parseRobots(body []byte) []*regexp.Regexp {
	var rules []*regexp.Regexp
	applies, inGroup := false, false
	scanner := bufio.NewScanner(bytes.NewReader(body))
	for scanner.Scan() {
		line := scanner.Text()
		if index := strings.IndexByte(line, '#'); index >= 0 {
			line = line[:index]
		}
		key, value, ok := strings.Cut(line, ":")
		if !ok {
			continue
		}
		key, value = strings.ToLower(strings.TrimSpace(key)), strings.TrimSpace(value)
		switch key {
		case "user-agent":
			if !inGroup {
				applies = false
			}
			inGroup = true
			if value == "*" {
				applies = true
			}
		case "disallow":
			inGroup = false
			if applies && value != "" {
				pattern := "^" + strings.ReplaceAll(regexp.QuoteMeta(strings.TrimSuffix(value, "$")), `\*`, ".*")
				if strings.HasSuffix(value, "$") {
					pattern += "$"
				}
				if rule, err := regexp.Compile(pattern); err == nil {
					rules = append(rules, rule)
				}
			}
		default:
			inGroup = false
		}
	}
	return rules
}

// fetchPageBody fetches a page and returns its entry and stored raw body.
func (p *WebPages) fetchPageBody(ctx context.Context, config Config, page Page, rawDir string) (pageEntry, []byte) {
	entry := p.fetchPage(ctx, config, page, rawDir)
	if entry.Status != statusOK {
		return entry, nil
	}
	body, err := readRaw(rawDir, entry)
	if err != nil {
		entry.Status, entry.Error = statusFailed, err.Error()
	}
	return entry, body
}

func readRaw(rawDir string, entry pageEntry) ([]byte, error) {
	return os.ReadFile(filepath.Join(rawDir, entry.File))
}

// contentDigest hashes the text a page contributes to the index, so pages
// that differ only in site navigation compare equal.
func contentDigest(body []byte, entry pageEntry) string {
	text := string(body)
	if entry.Type == TypeHTML {
		text, _ = PageMarkdown(body, "")
	}
	text = strings.Join(strings.Fields(text), " ")
	if len(text) < 200 {
		return ""
	}
	sum := sha256.Sum256([]byte(text))
	return hex.EncodeToString(sum[:])
}
