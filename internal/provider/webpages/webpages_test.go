package webpages

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/lkarlslund/knowledge-mcp/internal/knowledgeindex"
	"github.com/lkarlslund/knowledge-mcp/internal/model"
	sourceprovider "github.com/lkarlslund/knowledge-mcp/internal/provider"
)

// testRulesPage mirrors the structure of DBU regional rule pages: a site
// header and navigation, a main element with breadcrumb and share widgets, a
// vertical sub-navigation, and the article body with a rule table.
const testRulesPage = `<!DOCTYPE html><html lang="da"><head><meta charset="utf-8"><title>Turneringsbestemmelser | DBU Jylland</title><style>.x{}</style></head>
<body>
<header class="main-header"><a href="/">DBU Jylland</a><nav class="navigation--service-right"><a href="/login">Log ind</a></nav></header>
<main class="main contentpage">
  <div class="container">
    <div class="page--header"><ul class="breadcrumb"><li><a href="/">Forside</a></li><li>Regler</li></ul><div class="share-print-wrap"><button>Del</button></div></div>
    <div class="dbu-grid main-content hasNav">
      <div id="VerticalNav"><a href="/andre">Andre regler</a></div>
      <div class="main-content-container">
        <h1 class="header hyphenate">11:11 og 8:8 Herrer og Drenge</h1>
        <div class="rteModule">
          <p>Kampene spilles efter <strong>DBU's</strong> love. Se <a href="../turneringsreglement/">turneringsreglementet</a>.</p>
          <h2>§ 3 Spilletid</h2>
          <table><tr><th>Række</th><th>Spilletid</th></tr><tr><td>U15</td><td>2 x 35 min.</td></tr><tr><td>U17 | U19</td><td>2 x 45 min.</td></tr></table>
          <script>track()</script>
        </div>
      </div>
    </div>
  </div>
</main>
<footer class="main-footer"><nav><a href="/kontakt">Kontakt</a></nav></footer>
</body></html>`

const testLandingPage = `<html><body><main><h1>Love og regler</h1>
<div id="turneringsregler-for-herre-dm-2026-2027"><h3>Turneringsregler for Herre-DM 2026-2027</h3><a href="https://cms.example.test/assets/uuid-herre">Download</a></div>
<div id="turneringsregler-for-ungdoms-dm-2026-2027"><h3>Turneringsregler for Ungdoms-DM 2026-2027</h3><a href="/assets/uuid-ungdom">Download</a></div>
<p><a href="/files/cirkulaere.pdf">Cirkulære nr. 47</a></p>
</main></body></html>`

func TestParseConfigValidation(t *testing.T) {
	t.Parallel()
	config, err := ParseConfig([]byte("pages:\n  - slug: a\n    url: https://example.test/a\n  - {slug: b, url: 'https://example.test/b.pdf', type: PDF}\n"))
	if err != nil {
		t.Fatal(err)
	}
	if config.Pages[0].Type != TypeHTML || config.Pages[1].Type != TypePDF || config.Concurrency != defaultConcurrency || config.delay() != defaultDelay || config.userAgent() != DefaultUserAgent {
		t.Fatalf("defaults = %#v", config)
	}
	jsonConfig, err := ParseConfig([]byte(`{"name":"JSON list","concurrency":9,"delay":"250ms","pages":[{"slug":"x","title":"X","url":"http://example.test/x"}]}`))
	if err != nil || jsonConfig.Name != "JSON list" || jsonConfig.Concurrency != maxConcurrency || jsonConfig.delay() != 250*time.Millisecond {
		t.Fatalf("json config = %#v, %v", jsonConfig, err)
	}
	for name, document := range map[string]string{
		"empty":          "pages: []\n",
		"duplicate slug": "pages:\n  - {slug: a, url: 'https://e.test/1'}\n  - {slug: a, url: 'https://e.test/2'}\n",
		"bad slug":       "pages:\n  - {slug: 'Bad Slug', url: 'https://e.test/1'}\n",
		"bad scheme":     "pages:\n  - {slug: a, url: 'file:///etc/passwd'}\n",
		"bad type":       "pages:\n  - {slug: a, url: 'https://e.test/1', type: C}\n",
		"bad delay":      "delay: soon\npages:\n  - {slug: a, url: 'https://e.test/1'}\n",
	} {
		if _, err := ParseConfig([]byte(document)); err == nil {
			t.Errorf("%s: expected validation error", name)
		}
	}
}

func TestPageMarkdownExtractsMainContent(t *testing.T) {
	t.Parallel()
	body, title := PageMarkdown([]byte(testRulesPage), "https://www.dbujylland.dk/regler/turneringsbestemmelser/")
	if title != "11:11 og 8:8 Herrer og Drenge" {
		t.Fatalf("title = %q", title)
	}
	for _, want := range []string{
		"# 11:11 og 8:8 Herrer og Drenge",
		"**DBU's**",
		"[turneringsreglementet](https://www.dbujylland.dk/regler/turneringsreglement/)",
		"## § 3 Spilletid",
		"| Række | Spilletid |",
		"| U17 \\| U19 | 2 x 45 min. |",
	} {
		if !strings.Contains(body, want) {
			t.Fatalf("markdown missing %q:\n%s", want, body)
		}
	}
	for _, unwanted := range []string{"Log ind", "Forside", "Del", "Andre regler", "Kontakt", "track()", ".x{}"} {
		if strings.Contains(body, unwanted) {
			t.Fatalf("markdown contains boilerplate %q:\n%s", unwanted, body)
		}
	}
	// Without a main element the body is used, and the <title> is a fallback.
	body, title = PageMarkdown([]byte(`<html><head><title>Only title</title></head><body><p>Plain</p></body></html>`), "https://e.test/")
	if title != "Only title" || body != "Plain" {
		t.Fatalf("fallback = %q %q", title, body)
	}
}

func TestFindPDFLink(t *testing.T) {
	t.Parallel()
	cases := map[string]string{
		"Turneringsregler for Herre-DM 2026-2027": "https://cms.example.test/assets/uuid-herre",
		"Ungdoms-DM":       "https://divisionsforeningen.test/assets/uuid-ungdom",
		"Cirkulære nr. 47": "https://divisionsforeningen.test/files/cirkulaere.pdf",
	}
	for want, expected := range cases {
		got, err := findPDFLink([]byte(testLandingPage), "https://divisionsforeningen.test/love-og-regler", want)
		if err != nil || got != expected {
			t.Errorf("findPDFLink(%q) = %q, %v; want %q", want, got, err, expected)
		}
	}
	if _, err := findPDFLink([]byte(testLandingPage), "https://divisionsforeningen.test/", "Kvinde-DM"); err == nil {
		t.Error("missing document must fail")
	}
}

type siteState struct {
	mu       sync.Mutex
	failing  map[string]bool
	requests map[string]int
	agents   map[string]bool
}

func (s *siteState) count(path string) int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.requests[path]
}

func (s *siteState) sawAgent(agent string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.agents[agent]
}

func newTestSite(t *testing.T) (*httptest.Server, *siteState) {
	t.Helper()
	state := &siteState{failing: map[string]bool{}, requests: map[string]int{}, agents: map[string]bool{}}
	server := httptest.NewServer(http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
		state.mu.Lock()
		state.requests[request.URL.Path]++
		state.agents[request.UserAgent()] = true
		failing := state.failing[request.URL.Path]
		state.mu.Unlock()
		if failing {
			http.Error(response, "unavailable", http.StatusServiceUnavailable)
			return
		}
		switch request.URL.Path {
		case "/rules/":
			response.Header().Set("Content-Type", "text/html; charset=iso-8859-1")
			// "Spilletid på banen" encoded as Latin-1.
			_, _ = response.Write([]byte("<html><body><main><h1>Spilletid p\xe5 banen</h1><p>Kampe varer 2 x 45 minutter.</p></main></body></html>"))
		case "/moved":
			http.Redirect(response, request, "/rules/", http.StatusFound)
		case "/landing":
			response.Header().Set("Content-Type", "text/html; charset=utf-8")
			_, _ = response.Write([]byte(testLandingPage))
		case "/direct-pdf":
			response.Header().Set("Content-Type", "application/pdf")
			_, _ = response.Write([]byte("%PDF-1.7"))
		default:
			http.NotFound(response, request)
		}
	}))
	return server, state
}

func TestWebPagesLifecycle(t *testing.T) {
	t.Parallel()
	server, state := newTestSite(t)
	defer server.Close()
	configDir, dataDir := t.TempDir(), t.TempDir()
	list := `name: Test rules
description: Football rules for testing.
language: da
topics: [football, rules]
delay: 0s
pages:
  - {slug: spilletid, title: "", url: "` + server.URL + `/rules/"}
  - {slug: moved, title: "Flyttet side", url: "` + server.URL + `/moved"}
  - {slug: missing, title: "Findes ikke", url: "` + server.URL + `/missing"}
  - {slug: ungdoms-dm, title: "Ungdoms-DM regler", url: "` + server.URL + `/landing", type: pdf, pdf_link_text: "Ungdoms-DM"}
  - {slug: direct, title: "Direkte PDF", url: "` + server.URL + `/direct-pdf"}
`
	if err := os.WriteFile(filepath.Join(configDir, "test-rules.yaml"), []byte(list), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(configDir, "broken.yaml"), []byte("pages: []"), 0o644); err != nil {
		t.Fatal(err)
	}
	backend := New(configDir, dataDir)
	backend.sleep = func(ctx context.Context, _ time.Duration) error { return ctx.Err() }
	clock := time.Date(2026, 9, 16, 12, 0, 0, 0, time.UTC)
	backend.now = func() time.Time { return clock }
	ctx := context.Background()

	available, err := backend.Discover(ctx, "football", false)
	if err != nil || len(available) != 1 || available[0].ID != "test-rules" || available[0].Language.Code != "da" || available[0].PartCount != 5 {
		t.Fatalf("discover = %#v, %v", available, err)
	}
	if !backend.Owns("test-rules") || backend.Owns("rfc") || backend.Owns("../etc") {
		t.Fatal("ownership is wrong")
	}
	release, err := backend.Latest(ctx, "test-rules", "")
	if err != nil {
		t.Fatal(err)
	}
	sameWeek := clock.Add(48 * time.Hour)
	backend.now = func() time.Time { return sameWeek }
	again, _ := backend.Latest(ctx, "test-rules", "")
	backend.now = func() time.Time { return clock.Add(7 * 24 * time.Hour) }
	nextWeek, _ := backend.Latest(ctx, "test-rules", "")
	backend.now = func() time.Time { return clock }
	if again.Fingerprint != release.Fingerprint || nextWeek.Fingerprint == release.Fingerprint {
		t.Fatalf("fingerprints: same week %v, next week %v", again.Fingerprint == release.Fingerprint, nextWeek.Fingerprint == release.Fingerprint)
	}

	stage := t.TempDir()
	var messages []string
	manifest, err := backend.Acquire(ctx, "test-rules", "html", release, stage, "", func(_ string, _, _ int64, _ string, _ float64, message string) {
		messages = append(messages, message)
	})
	if err != nil {
		t.Fatal(err)
	}
	if manifest.DocumentCount != 4 || manifest.PartCount != 5 || manifest.Site.Language.Code != "da" {
		t.Fatalf("manifest = %#v", manifest)
	}
	if state.count("/missing") != 1 {
		t.Fatalf("a 404 must not be retried: %d", state.count("/missing"))
	}
	if !strings.Contains(messages[len(messages)-1], "1 of 5 pages unavailable") {
		t.Fatalf("failure not reported: %v", messages)
	}
	if !state.sawAgent(DefaultUserAgent) {
		t.Fatal("default user agent was not sent")
	}
	index, err := readIndex(stage)
	if err != nil {
		t.Fatal(err)
	}
	statuses := map[string]pageEntry{}
	for _, entry := range index.Entries {
		statuses[entry.Slug] = entry
	}
	if statuses["missing"].Status != statusFailed || !strings.Contains(statuses["missing"].Error, "HTTP 404") {
		t.Fatalf("missing = %#v", statuses["missing"])
	}
	if statuses["ungdoms-dm"].ResolvedURL != server.URL+"/assets/uuid-ungdom" || statuses["direct"].Type != TypePDF || statuses["moved"].ResolvedURL != server.URL+"/rules/" {
		t.Fatalf("statuses = %#v", statuses)
	}

	corpus, err := backend.OpenCorpus(stage, manifest)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = corpus.Close() }()
	count, err := knowledgeindex.BuildTitle(ctx, stage, manifest.Fingerprint, corpus, sourceprovider.ScanOptions{}, func(uint64, int64, int64) {})
	if err != nil || count != 4 {
		t.Fatalf("title index = %d, %v", count, err)
	}
	if err := os.Rename(filepath.Join(stage, knowledgeindex.TitleDirectory+".building"), filepath.Join(stage, knowledgeindex.TitleDirectory)); err != nil {
		t.Fatal(err)
	}
	if err := knowledgeindex.BuildBody(ctx, stage, manifest.Fingerprint, corpus, sourceprovider.ScanOptions{}, func(uint64, int64, int64, string) {}); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(filepath.Join(stage, knowledgeindex.BodyDirectory+".building"), filepath.Join(stage, knowledgeindex.BodyDirectory)); err != nil {
		t.Fatal(err)
	}
	reader, err := knowledgeindex.Open(stage, corpus, true)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = reader.Close() }()
	result, err := reader.Search(ctx, "minutter", model.SearchOptions{Limit: 5, Snippets: true}, true)
	if err != nil || len(result.Hits) == 0 || (result.Hits[0].ID != "spilletid" && result.Hits[0].ID != "moved") || !strings.Contains(result.Hits[0].Snippet, "45 minutter") {
		t.Fatalf("search = %#v, %v", result, err)
	}
	document, err := reader.Read(ctx, "", "spilletid", model.ReadOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if document.Title != "Spilletid på banen" || !strings.Contains(document.Content, "**Source:** "+server.URL+"/rules/") || !strings.Contains(document.Content, "# Spilletid på banen") {
		t.Fatalf("document = %#v", document)
	}
	pdf, err := reader.Read(ctx, "", "ungdoms-dm", model.ReadOptions{})
	if err != nil || !strings.Contains(pdf.Content, "PDF document: [Ungdoms-DM regler]("+server.URL+"/assets/uuid-ungdom)") {
		t.Fatalf("pdf = %#v, %v", pdf, err)
	}

	// An update where a page now fails keeps the previous copy as stale.
	state.mu.Lock()
	state.failing["/rules/"] = true
	state.mu.Unlock()
	next := t.TempDir()
	updated, err := backend.Acquire(ctx, "test-rules", "html", nextWeek, next, stage, func(string, int64, int64, string, float64, string) {})
	if err != nil {
		t.Fatal(err)
	}
	if updated.DocumentCount != 4 || state.count("/rules/") < 1+fetchAttempts {
		t.Fatalf("update = %#v requests=%d", updated, state.count("/rules/"))
	}
	nextCorpus, err := backend.OpenCorpus(next, updated)
	if err != nil {
		t.Fatal(err)
	}
	stale, err := nextCorpus.Read(ctx, sourceprovider.Record{ID: "spilletid"}, model.ReadOptions{})
	if err != nil || !strings.Contains(stale.Content, "latest refetch failed") || !strings.Contains(stale.Content, "45 minutter") {
		t.Fatalf("stale = %#v, %v", stale, err)
	}

	// Once installed, the dataset stays owned even if its list is removed.
	if err := os.MkdirAll(filepath.Join(dataDir, "test-rules"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dataDir, "test-rules", "manifest.json"), []byte("{}"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(filepath.Join(configDir, "test-rules.yaml")); err != nil {
		t.Fatal(err)
	}
	if !backend.Owns("test-rules") {
		t.Fatal("installed dataset lost its owner")
	}
	if _, err := backend.Latest(ctx, "test-rules", ""); err == nil {
		t.Fatal("latest without a list must fail")
	}
}

func TestAcquireFailsWhenNothingFetched(t *testing.T) {
	t.Parallel()
	server, _ := newTestSite(t)
	defer server.Close()
	configDir := t.TempDir()
	if err := os.WriteFile(filepath.Join(configDir, "gone.json"), []byte(`{"delay":"0s","pages":[{"slug":"a","url":"`+server.URL+`/nope"}]}`), 0o644); err != nil {
		t.Fatal(err)
	}
	backend := New(configDir, "")
	release, err := backend.Latest(context.Background(), "gone", "html")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := backend.Acquire(context.Background(), "gone", "html", release, t.TempDir(), "", func(string, int64, int64, string, float64, string) {}); err == nil || !strings.Contains(err.Error(), "no pages could be fetched") {
		t.Fatalf("err = %v", err)
	}
}

func TestShippedDBUListParses(t *testing.T) {
	t.Parallel()
	data, err := os.ReadFile(filepath.Join("..", "..", "..", "contrib", "webpages", "dbu-rules.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	config, err := ParseConfig(data)
	if err != nil {
		t.Fatal(err)
	}
	pdfs := 0
	for _, page := range config.Pages {
		if page.Type == TypePDF {
			pdfs++
		}
	}
	if len(config.Pages) != 496 || pdfs != 4 || config.Language != "da" || config.Concurrency != 2 || config.delay() != time.Second {
		t.Fatalf("pages=%d pdfs=%d config=%s/%d/%s", len(config.Pages), pdfs, config.Language, config.Concurrency, config.Delay)
	}
}

func TestShippedADSecurityListParses(t *testing.T) {
	t.Parallel()
	data, err := os.ReadFile(filepath.Join("..", "..", "..", "contrib", "webpages", "ad-security-references.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	config, err := ParseConfig(data)
	if err != nil {
		t.Fatal(err)
	}
	pdfs, mitre := 0, 0
	for _, page := range config.Pages {
		if page.Type == TypePDF {
			pdfs++
		}
		if strings.HasPrefix(page.URL, "https://attack.mitre.org/techniques/") {
			mitre++
		}
	}
	if len(config.Pages) != 85 || pdfs != 1 || mitre != 52 || config.Language != "en" || config.Concurrency != 2 || config.delay() != time.Second {
		t.Fatalf("pages=%d pdfs=%d mitre=%d config=%s/%d/%s", len(config.Pages), pdfs, mitre, config.Language, config.Concurrency, config.Delay)
	}
}

func TestSourceFileManagement(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	backend := New(dir, "")
	valid := "name: Mine\npages:\n  - {slug: one, title: One, url: \"https://example.com/one\"}\n"
	if err := backend.SaveSource("mine", valid); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(dir, "mine.yaml")); err != nil {
		t.Fatalf("new list not written as .yaml: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, "legacy.json"), []byte(`{"pages":[{"slug":"a","title":"A","url":"https://example.com/a"}]}`), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := backend.SaveSource("legacy", valid); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(dir, "legacy.yaml")); err == nil {
		t.Fatal("saving an existing .json list created a second .yaml file")
	}
	if err := os.WriteFile(filepath.Join(dir, "broken.yaml"), []byte("pages: []\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	sources, err := backend.Sources()
	if err != nil {
		t.Fatal(err)
	}
	if len(sources) != 3 || sources[0].ID != "broken" || sources[0].Err == nil || sources[2].ID != "mine" || len(sources[2].Config.Pages) != 1 || sources[1].File != "legacy.json" {
		t.Fatalf("sources = %+v", sources)
	}
	for name, content := range map[string]string{"mine": "pages: []\n", "../escape": valid, "Upper": valid} {
		if err := backend.SaveSource(name, content); err == nil {
			t.Fatalf("SaveSource(%q) succeeded", name)
		}
	}
	if content, err := backend.ReadSource("mine"); err != nil || content != valid {
		t.Fatalf("rejected save changed the file: %q, %v", content, err)
	}
	if err := backend.DeleteSource("mine"); err != nil {
		t.Fatal(err)
	}
	if backend.Owns("mine") {
		t.Fatal("deleted list is still owned")
	}
	if err := backend.DeleteSource("../escape"); err == nil {
		t.Fatal("deleting an invalid name succeeded")
	}
}
