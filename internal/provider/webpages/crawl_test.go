package webpages

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/lkarlslund/knowledge-mcp/internal/provider"
)

var longText = strings.Repeat("Kampen spilles efter Fodboldloven. ", 12)

func newCrawlSite(t *testing.T) (*httptest.Server, map[string]int, *sync.Mutex) {
	t.Helper()
	requests := map[string]int{}
	var mu sync.Mutex
	var server *httptest.Server
	server = httptest.NewServer(http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
		mu.Lock()
		requests[request.URL.RequestURI()]++
		mu.Unlock()
		htmlPage := func(body string) {
			response.Header().Set("Content-Type", "text/html; charset=utf-8")
			_, _ = response.Write([]byte("<html><body><main>" + body + "</main></body></html>"))
		}
		switch request.URL.Path {
		case "/robots.txt":
			_, _ = response.Write([]byte("User-agent: *\nDisallow: /regler/privat\nDisallow: /*?print=\n"))
		case "/sitemap.xml":
			_, _ = response.Write([]byte(`<?xml version="1.0"?><urlset><url><loc>` + server.URL + `/regler/fra-sitemap</loc></url><url><loc>` + server.URL + `/nyheder/x</loc></url></urlset>`))
		case "/regler/":
			htmlPage(`<h1>Regler</h1><a href="/regler/spilletid/#top">Spilletid</a> <a href="spilletid">igen</a>
<a href="/regler/privat/a">privat</a> <a href="/regler/spilletid?print=1">print</a> <a href="/nyheder/">nyheder</a>
<a href="/regler/original">original</a> <a href="/regler/arkiv/gammel">arkiv</a> <a href="/assets/abc-123">Ungdoms-DM regler</a> <a href="/regler/billede.png">billede</a>
<a href="mailto:x@example.test">mail</a> <a href="/regler/dyb/1">dyb</a> <a href="/assets/u16">Download</a> <a href="/regler/spejl">spejl</a>`)
		case "/regler/spilletid", "/regler/spilletid/":
			htmlPage(`<h1>Spilletid</h1><p>U15 spiller 2x40 minutter.</p><a href="/regler/">tilbage</a> <a href="/regler/kun-fra-statisk">kun her</a>`)
		case "/regler/kun-fra-statisk":
			htmlPage(`<h1>Kun linket fra den statiske side</h1><p>` + longText + `</p>`)
		case "/regler/original":
			htmlPage(`<nav>Menu A</nav><h1>Fodboldloven</h1><p>` + longText + `</p>`)
		case "/regler/spejl":
			htmlPage(`<nav>Menu B</nav><h1>Fodboldloven</h1><p>` + longText + `</p>`)
		case "/assets/u16":
			response.Header().Set("Content-Type", "application/pdf")
			response.Header().Set("Content-Disposition", `inline; filename="Turneringsregler_U16-Cup 2026-2027.pdf"`)
			_, _ = response.Write([]byte("%PDF-u16"))
		case "/regler/fra-sitemap":
			htmlPage(`<h1>Fra sitemap</h1><p>Sitemap side.</p>`)
		case "/regler/dyb/1":
			htmlPage(`<h1>Dyb 1</h1><a href="/regler/dyb/2">næste</a>`)
		case "/regler/dyb/2":
			htmlPage(`<h1>Dyb 2</h1><a href="/regler/dyb/3">næste</a>`)
		case "/regler/billede.png":
			response.Header().Set("Content-Type", "image/png")
			_, _ = response.Write([]byte("PNG"))
		case "/assets/abc-123":
			response.Header().Set("Content-Type", "application/octet-stream")
			_, _ = response.Write([]byte("%PDF-rules"))
		default:
			http.NotFound(response, request)
		}
	}))
	return server, requests, &mu
}

func TestCrawlDiscoversScopedPagesAndDocuments(t *testing.T) {
	t.Parallel()
	server, requests, mu := newCrawlSite(t)
	defer server.Close()
	configDir, dataDir := t.TempDir(), t.TempDir()
	list := `name: Crawled rules
language: da
delay: 0s
pages:
  - {slug: static-spilletid, title: "Spilletid (statisk)", url: "` + server.URL + `/regler/spilletid"}
crawl:
  - start: ["` + server.URL + `/regler/"]
    sitemaps: ["` + server.URL + `/sitemap.xml"]
    include: ["` + server.URL + `/regler/"]
    exclude: ["/arkiv/"]
    documents: ["` + server.URL + `/assets/"]
    max_depth: 2
`
	if err := os.WriteFile(filepath.Join(configDir, "crawled.yaml"), []byte(list), 0o644); err != nil {
		t.Fatal(err)
	}
	backend := New(configDir, dataDir)
	backend.sleep = func(ctx context.Context, _ time.Duration) error { return ctx.Err() }
	backend.pdfText = func(_ context.Context, data []byte) (string, error) {
		return "§ 27.1 Spilletid 2x40 (" + string(data) + ")", nil
	}
	ctx := context.Background()
	release, err := backend.Latest(ctx, "crawled", "")
	if err != nil {
		t.Fatal(err)
	}
	stage := t.TempDir()
	manifest, err := backend.Acquire(ctx, "crawled", "html", release, stage, "", func(string, int64, int64, string, float64, string) {})
	if err != nil {
		t.Fatal(err)
	}
	index, err := readIndex(stage)
	if err != nil {
		t.Fatal(err)
	}
	var urls []string
	byPath := map[string]pageEntry{}
	for _, entry := range index.Entries {
		path := strings.TrimPrefix(entry.URL, server.URL)
		urls = append(urls, path)
		byPath[path] = entry
		if entry.Status != statusOK {
			t.Errorf("%s: %s %s", path, entry.Status, entry.Error)
		}
	}
	sort.Strings(urls)
	want := []string{"/assets/abc-123", "/assets/u16", "/regler/", "/regler/dyb/1", "/regler/dyb/2", "/regler/fra-sitemap", "/regler/kun-fra-statisk", "/regler/original", "/regler/spilletid"}
	if strings.Join(urls, " ") != strings.Join(want, " ") {
		t.Fatalf("pages = %v, want %v", urls, want)
	}
	if manifest.DocumentCount != uint64(len(want)) || manifest.PartCount != len(want) {
		t.Fatalf("manifest counts = %d/%d", manifest.DocumentCount, manifest.PartCount)
	}
	// The static page keeps its slug and title; the linked asset is a PDF
	// titled by its anchor text; crawled slugs are derived from the URL.
	if byPath["/regler/spilletid"].Slug != "static-spilletid" || byPath["/assets/abc-123"].Type != TypePDF || byPath["/assets/abc-123"].Title != "Ungdoms-DM regler" || byPath["/assets/abc-123"].File != byPath["/assets/abc-123"].Slug+pdfTextSuffix {
		t.Fatalf("entries = %#v", byPath)
	}
	if slug := byPath["/regler/dyb/1"].Slug; slug != "127-regler-dyb-1" {
		t.Fatalf("crawled slug = %q", slug)
	}
	mu.Lock()
	defer mu.Unlock()
	if urlKey("https://A.test:443/x/?q=1") != "https://a.test/x?q=1" || urlKey("https://a.test/") != "https://a.test/" || urlKey("https://a.test") != "https://a.test/" {
		t.Errorf("urlKey = %q %q %q", urlKey("https://A.test:443/x/?q=1"), urlKey("https://a.test/"), urlKey("https://a.test"))
	}
	for name, want := range map[string]string{"Cirkulaere-nr.-13.pdf": "Cirkulaere nr. 13", "Regler%20U15.pdf": "Regler U15", "x": "x"} {
		if got := titleFromFilename(name); got != want {
			t.Errorf("titleFromFilename(%q) = %q, want %q", name, got, want)
		}
	}
	if byPath["/assets/u16"].Title != "Turneringsregler U16-Cup 2026-2027" {
		t.Fatalf("generic link title not replaced by the file name: %q", byPath["/assets/u16"].Title)
	}
	// The statically listed page is fetched once, during the crawl, and its
	// links are followed; the mirror of /regler/original is dropped.
	for _, path := range []string{"/regler/", "/regler/dyb/1", "/assets/abc-123", "/regler/spilletid", "/regler/spejl"} {
		if requests[path] != 1 {
			t.Errorf("%s fetched %d times, want exactly once", path, requests[path])
		}
	}
	for _, path := range []string{"/regler/privat/a", "/regler/spilletid?print=1", "/nyheder/", "/nyheder/x", "/regler/arkiv/gammel", "/regler/dyb/3"} {
		if requests[path] != 0 {
			t.Errorf("%s was fetched although robots, scope, exclude or depth forbid it", path)
		}
	}

	// The corpus serves the extracted PDF text.
	corpus, err := backend.OpenCorpus(stage, manifest)
	if err != nil {
		t.Fatal(err)
	}
	var bodies []string
	if err := corpus.ScanBodies(ctx, "", provider.ScanOptions{}, func(record provider.Record, _ provider.ScanPosition) error {
		bodies = append(bodies, record.Body)
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(strings.Join(bodies, "\n"), "**§ 27.1** Spilletid 2x40 (%PDF-rules)") {
		t.Fatalf("PDF text missing from bodies: %q", bodies)
	}
}

func TestCrawlValidationAndHelpers(t *testing.T) {
	t.Parallel()
	for _, bad := range []string{
		"crawl:\n  - include: [https://a.test/]\n",
		"crawl:\n  - start: [https://a.test/x]\n",
		"crawl:\n  - start: [https://b.test/]\n    include: [https://a.test/]\n",
		"crawl:\n  - start: [ftp://a.test/]\n    include: [ftp://a.test/]\n",
	} {
		if _, err := ParseConfig([]byte(bad)); err == nil {
			t.Errorf("ParseConfig accepted %q", bad)
		}
	}
	config, err := ParseConfig([]byte("crawl:\n  - start: [https://a.test/r/]\n    include: [https://a.test/r/]\n"))
	if err != nil || config.Crawl[0].MaxPages != defaultCrawlPages || config.Crawl[0].MaxDepth != defaultCrawlDepth || sourceSite(config) != "https://a.test/" {
		t.Fatalf("config = %#v, %v", config, err)
	}
	used := map[string]string{}
	first := slugForURL("https://www.dbujylland.dk/Turneringer/Sådan-spiller-vi/", used)
	again := slugForURL("https://www.dbujylland.dk/Turneringer/Sådan-spiller-vi/", used)
	clash := slugForURL("https://www.dbujylland.dk/turneringer/saadan/spiller/vi", used)
	if first != "dbujylland-turneringer-saadan-spiller-vi" || again != first || clash == first || !idPattern.MatchString(clash) {
		t.Fatalf("slugs = %q %q %q", first, again, clash)
	}
	long := slugForURL("https://a.test/"+strings.Repeat("x", 300), used)
	if !idPattern.MatchString(long) {
		t.Fatalf("long slug %q is invalid", long)
	}
	rules := parseRobots([]byte("User-agent: Googlebot\nDisallow: /\n\nUser-agent: *\nDisallow: /admin\nDisallow: /*.pdf$\nAllow: /x\n"))
	matches := func(target string) bool {
		for _, rule := range rules {
			if rule.MatchString(target) {
				return true
			}
		}
		return false
	}
	if len(rules) != 2 || !matches("/admin/x") || !matches("/a/b.pdf") || matches("/a/b.pdf?x") || matches("/regler") {
		t.Fatalf("robots rules = %v", rules)
	}
}
