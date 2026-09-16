package gitdocs

import (
	"archive/tar"
	"bytes"
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/klauspost/compress/gzip"
	"github.com/lkarlslund/knowledge-mcp/internal/knowledgeindex"
	"github.com/lkarlslund/knowledge-mcp/internal/model"
	sourceprovider "github.com/lkarlslund/knowledge-mcp/internal/provider"
)

const testCommit = "0123456789abcdef0123456789abcdef01234567"

func TestBuiltinCatalog(t *testing.T) {
	t.Parallel()
	datasets, err := ParseCatalog(builtinCatalog)
	if err != nil {
		t.Fatal(err)
	}
	if len(datasets) != 24 {
		t.Fatalf("datasets = %d, want 24", len(datasets))
	}
	byID := map[string]Dataset{}
	for _, item := range datasets {
		byID[item.ID] = item
	}
	entra, ok := byID["microsoftdocs-entra-docs"]
	if !ok || len(entra.Fragments) == 0 || entra.SiteRoot != "https://learn.microsoft.com" || entra.Project != "Microsoft Learn" {
		t.Fatalf("entra inherits Microsoft Learn defaults: %#v", entra)
	}
	for _, id := range []string{"microsoftdocs-azure-docs", "home-assistant-home-assistant.io", "home-assistant-developers.home-assistant", "specterops-bloodhound", "specterops-bloodhound-docs", "the-hacker-recipes-the-hacker-recipes", "maester365-maester"} {
		if _, ok := byID[id]; !ok {
			t.Fatalf("missing dataset %q", id)
		}
	}
	if byID["specterops-sharphound"].Include[0] != "**/*.md" {
		t.Fatalf("tool repo defaults not applied: %#v", byID["specterops-sharphound"])
	}
	if _, err := ParseCatalog([]byte("datasets:\n  - repo: a/b\n    include: [x]\n  - repo: A/B\n    include: [y]\n")); err == nil {
		t.Fatal("duplicate derived IDs must be rejected")
	}
	if _, err := ParseCatalog([]byte("datasets:\n  - repo: not-a-repo\n    include: [x]\n")); err == nil {
		t.Fatal("invalid repo must be rejected")
	}
	if _, err := ParseCatalog([]byte("datasets:\n  - repo: a/b\n")); err == nil {
		t.Fatal("missing include must be rejected")
	}
}

func TestCitationURLMapping(t *testing.T) {
	t.Parallel()
	datasets, err := ParseCatalog(builtinCatalog)
	if err != nil {
		t.Fatal(err)
	}
	find := func(id string) *Dataset {
		for index := range datasets {
			if datasets[index].ID == id {
				return &datasets[index]
			}
		}
		t.Fatalf("dataset %q missing", id)
		return nil
	}
	cases := []struct {
		dataset, path, want string
	}{
		{"microsoftdocs-entra-docs", "docs/identity/authentication/concept-mfa-howitworks.md", "https://learn.microsoft.com/en-us/entra/identity/authentication/concept-mfa-howitworks"},
		{"microsoftdocs-entra-docs", "docs/identity/index.yml", "https://learn.microsoft.com/en-us/entra/identity/"},
		{"microsoftdocs-powershell-docs", "reference/7.6/Microsoft.PowerShell.Core/About/about_Scopes.md", "https://learn.microsoft.com/en-us/powershell/module/microsoft.powershell.core/about/about_scopes?view=powershell-7.6"},
		{"microsoftdocs-powershell-docs", "reference/docs-conceptual/learn/ps101/01-getting-started.md", "https://learn.microsoft.com/en-us/powershell/scripting/learn/ps101/01-getting-started"},
		{"microsoftdocs-supportarticles-docs", "SharePoint/SharePointOnline/web-parts/change-summary-link-web-part.md", "https://learn.microsoft.com/en-us/troubleshoot/sharepoint/web-parts/change-summary-link-web-part"},
		{"microsoftdocs-supportarticles-docs", "support/windows-server/active-directory/replication-error-8453.md", "https://learn.microsoft.com/en-us/troubleshoot/windows-server/active-directory/replication-error-8453"},
		{"microsoftdocs-defender-docs", "defender-for-cloud-apps/what-is-defender-for-cloud-apps.md", "https://learn.microsoft.com/en-us/defender-cloud-apps/what-is-defender-for-cloud-apps"},
		{"microsoftdocs-defender-docs", "defender-for-cloud/secure-score.md", "https://learn.microsoft.com/en-us/azure/defender-for-cloud/secure-score"},
		{"home-assistant-home-assistant.io", "source/_integrations/hue.markdown", "https://www.home-assistant.io/integrations/hue/"},
		{"home-assistant-home-assistant.io", "source/_template_functions/today_at.markdown", "https://www.home-assistant.io/template-functions/today_at/"},
		{"home-assistant-home-assistant.io", "source/getting-started/index.markdown", "https://www.home-assistant.io/getting-started/"},
		{"home-assistant-developers.home-assistant", "docs/core/entity/light.md", "https://developers.home-assistant.io/docs/core/entity/light"},
		{"specterops-bloodhound-docs", "docs/analyze-data/configuration.mdx", "https://bloodhound.specterops.io/analyze-data/configuration"},
		{"the-hacker-recipes-the-hacker-recipes", "docs/src/ad/movement/adcs/certifried.md", "https://www.thehacker.recipes/ad/movement/adcs/certifried"},
		{"the-hacker-recipes-the-hacker-recipes", "README.md", "https://github.com/The-Hacker-Recipes/The-Hacker-Recipes/blob/" + testCommit + "/README.md"},
		{"specterops-bloodhound", "packages/go/My Package/README.md", "https://github.com/SpecterOps/BloodHound/blob/" + testCommit + "/packages/go/My%20Package/README.md"},
	}
	for _, test := range cases {
		if got := citationURL(find(test.dataset), testCommit, test.path); got != test.want {
			t.Errorf("citationURL(%s, %s) = %s, want %s", test.dataset, test.path, got, test.want)
		}
	}
	// A wildcard prefix needs a real segment after it: "Exchange/x.md" has no
	// sub-docset and falls back to the blob URL.
	if got := citationURL(find("microsoftdocs-supportarticles-docs"), testCommit, "Exchange/index.md"); !strings.HasPrefix(got, "https://github.com/") {
		t.Errorf("wildcard prefix without sub-docset = %s", got)
	}
}

func TestGlobMatchingAndSelection(t *testing.T) {
	t.Parallel()
	cases := []struct {
		pattern, name string
		want          bool
	}{
		{"**/*.md", "README.md", true},
		{"**/*.md", "a/b/c.md", true},
		{"**/includes/**", "includes/x.md", true},
		{"**/includes/**", "articles/includes/x.md", true},
		{"**/includes/**", "articles/x.md", false},
		{"README.md", "docs/README.md", false},
		{"reference/7.6/**/*.md", "reference/7.6/Mod/Get-X.md", true},
		{"reference/7.6/**/*.md", "reference/7.5/Mod/Get-X.md", false},
		{"LICENSE*", "LICENSE-CODE.md", true},
		{"source/_posts/**", "source/_posts/2024-01-01-x.markdown", true},
	}
	for _, test := range cases {
		if got := matchGlob(test.pattern, test.name); got != test.want {
			t.Errorf("matchGlob(%q, %q) = %v", test.pattern, test.name, got)
		}
	}
	item := &Dataset{Include: []string{"docs/**"}, Exclude: []string{"docs/private/**"}}
	for name, want := range map[string]bool{"docs/a.md": true, "docs/a.yml": true, "docs/a.png": false, "docs/private/a.md": false, "other/a.md": false, "docs/a.MARKDOWN": true} {
		if got := selectFile(item, name); got != want {
			t.Errorf("selectFile(%q) = %v, want %v", name, got, want)
		}
	}
}

type archiveFile struct {
	name     string
	body     string
	typeflag byte
	size     int64
}

func buildArchive(t *testing.T, files []archiveFile) []byte {
	t.Helper()
	var buffer bytes.Buffer
	compressed := gzip.NewWriter(&buffer)
	writer := tar.NewWriter(compressed)
	for _, file := range files {
		header := &tar.Header{Name: file.name, Mode: 0o644, Size: int64(len(file.body)), Typeflag: file.typeflag}
		if header.Typeflag == 0 {
			header.Typeflag = tar.TypeReg
		}
		if header.Typeflag == tar.TypeSymlink {
			header.Linkname, header.Size = "../../etc/passwd", 0
		}
		if header.Typeflag == tar.TypeDir {
			header.Size = 0
		}
		body := file.body
		if file.size > 0 {
			header.Size = file.size
			body = strings.Repeat("x", int(file.size))
		}
		if err := writer.WriteHeader(header); err != nil {
			t.Fatal(err)
		}
		if header.Size > 0 {
			if _, err := writer.Write([]byte(body)); err != nil {
				t.Fatal(err)
			}
		}
	}
	if err := writer.Close(); err != nil {
		t.Fatal(err)
	}
	if err := compressed.Close(); err != nil {
		t.Fatal(err)
	}
	return buffer.Bytes()
}

func TestExtractArchiveKeepsOnlySafeMatchingText(t *testing.T) {
	t.Parallel()
	root := "owner-repo-" + testCommit + "/"
	archive := buildArchive(t, []archiveFile{
		{name: root, typeflag: tar.TypeDir},
		{name: root + "docs/guide.md", body: "# Guide\n"},
		{name: root + "docs/media/diagram.png", body: "PNG"},
		{name: root + "docs/link.md", typeflag: tar.TypeSymlink},
		{name: root + "docs/../../escape.md", body: "# Escape\n"},
		{name: root + "docs/huge.md", size: maxTextFile + 1},
		{name: root + "src/main.go", body: "package main"},
		{name: "pax_global_header", body: "x"},
	})
	rawDir := t.TempDir()
	item := &Dataset{Include: []string{"docs/**"}}
	var reports int
	files, err := extractArchive(bytes.NewReader(archive), rawDir, func(name string) bool { return selectFile(item, name) }, func(int) { reports++ })
	if err != nil {
		t.Fatal(err)
	}
	if len(files) != 1 || files[0] != "docs/guide.md" || reports == 0 {
		t.Fatalf("files = %#v reports=%d", files, reports)
	}
	var kept []string
	_ = filepath.Walk(filepath.Dir(rawDir), func(path string, info os.FileInfo, err error) error {
		if err == nil && !info.IsDir() {
			kept = append(kept, path)
		}
		return nil
	})
	if len(kept) != 1 || kept[0] != filepath.Join(rawDir, "docs", "guide.md") {
		t.Fatalf("files on disk = %#v (path traversal, symlink, media, or oversize entry written)", kept)
	}
	if _, err := extractArchive(strings.NewReader("not gzip"), rawDir, func(string) bool { return true }, nil); err == nil {
		t.Fatal("corrupt archive must fail")
	}
}

func TestParseAdvertisedRef(t *testing.T) {
	t.Parallel()
	pkt := func(line string) string { return fmt.Sprintf("%04x%s", len(line)+4, line) }
	head := strings.Repeat("a", 40)
	branch := strings.Repeat("b", 40)
	data := pkt("# service=git-upload-pack\n") + "0000" + pkt(head+" HEAD\x00multi_ack symref=HEAD:refs/heads/main\n") + pkt(branch+" refs/heads/live\n") + "0000"
	if got, err := parseAdvertisedRef([]byte(data), "HEAD"); err != nil || got != head {
		t.Fatalf("HEAD = %q, %v", got, err)
	}
	if got, err := parseAdvertisedRef([]byte(data), "refs/heads/live"); err != nil || got != branch {
		t.Fatalf("branch = %q, %v", got, err)
	}
	if _, err := parseAdvertisedRef([]byte(data), "refs/heads/missing"); err == nil {
		t.Fatal("missing ref must fail")
	}
	if _, err := parseAdvertisedRef([]byte("zzzz"), "HEAD"); err == nil {
		t.Fatal("invalid pkt-line must fail")
	}
}

func TestDescribeFrontMatterTitlesAndYAML(t *testing.T) {
	t.Parallel()
	item := &Dataset{ID: "x", Repo: "o/r", Fragments: []string{"**/includes/**"}}
	entry, keep := describe(item, testCommit, "docs/a.md", "---\ntitle: Front title\nms.date: 01/31/2025\ndescription: Summary text\nms.author: someone\n---\n# Body heading\n")
	if !keep || entry.Title != "Front title" || entry.Metadata["ms.date"] != "01/31/2025" || entry.Metadata["description"] != "Summary text" || entry.Metadata["ms.author"] != "" || entry.Fragment {
		t.Fatalf("entry = %#v", entry)
	}
	entry, _ = describe(item, testCommit, "docs/b.md", "Intro\n\n# First *real* heading\n")
	if entry.Title != "First *real* heading" {
		t.Fatalf("heading title = %q", entry.Title)
	}
	entry, _ = describe(item, testCommit, "docs/network-setup/index.md", "no heading")
	if entry.Title != "network setup" {
		t.Fatalf("path title = %q", entry.Title)
	}
	entry, _ = describe(item, testCommit, "docs/includes/snippet.md", "Shared text")
	if !entry.Fragment {
		t.Fatal("include file must be a fragment")
	}
	if _, keep := describe(item, testCommit, "docs/index.yml", "### YamlMime:Landing\ntitle: Landing\n"); keep {
		t.Fatal("landing YAML must be skipped")
	}
	if _, keep := describe(item, testCommit, "docs/config.yml", "key: value\n"); keep {
		t.Fatal("non-DocFX YAML must be skipped")
	}
	faq := "### YamlMime:FAQ\nmetadata:\n  title: FAQ meta title\n  ms.date: 2024-02-03\ntitle: Frequently asked questions\nsummary: Common answers.\nsections:\n  - name: Licensing\n    questions:\n      - question: Do I need a license?\n        answer: Yes, a P1 license.\n"
	entry, keep = describe(item, testCommit, "docs/faq.yml", faq)
	if !keep || entry.Title != "Frequently asked questions" || entry.Metadata["yamlmime"] != "FAQ" || entry.Metadata["ms.date"] != "2024-02-03" {
		t.Fatalf("faq entry = %#v", entry)
	}
	rendered := yamlMarkdown(faq)
	for _, want := range []string{"# Frequently asked questions", "### Licensing", "### Do I need a license?", "Yes, a P1 license."} {
		if !strings.Contains(rendered, want) {
			t.Fatalf("faq markdown missing %q:\n%s", want, rendered)
		}
	}
	if strings.Contains(rendered, "FAQ meta title") {
		t.Fatalf("metadata leaked into body:\n%s", rendered)
	}
}

func TestGitDocsLifecycle(t *testing.T) {
	t.Parallel()
	root := "contoso-docs-" + testCommit + "/"
	archive := buildArchive(t, []archiveFile{
		{name: root + "README.md", body: "# Repo readme\n"},
		{name: root + "docs/includes/license-note.md", body: "---\nms.topic: include\n---\nConditional Access requires a **Premium P1** license.\n"},
		{name: root + "docs/identity/conditional-access.md", body: "---\ntitle: Plan Conditional Access\nms.date: 05/06/2025\ndescription: Planning guide\n---\n# Plan a Conditional Access deployment\n\n[!INCLUDE [license](../includes/license-note.md)]\n\n## Named locations\n\nUse trusted IP ranges. See [MFA](mfa.md#methods), [rooted](/docs/identity/mfa.md), [portal](/entra/fundamentals/whats-new), and ![diagram](media/ca.png).\n\n## Report-only mode\n\nEvaluate policies before enforcing.\n"},
		{name: root + "docs/identity/mfa.md", body: "# How multifactor authentication works\n\n## Methods\n\nAuthenticator app and FIDO2 keys.\n"},
		{name: root + "docs/identity/media/ca.png", body: "PNG"},
	})
	var apiCalls, gitCalls, archiveCalls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
		switch request.URL.Path {
		case "/api/repos/contoso/docs/commits/HEAD":
			apiCalls.Add(1)
			http.Error(response, "rate limited", http.StatusForbidden)
		case "/git/contoso/docs.git/info/refs":
			gitCalls.Add(1)
			line := testCommit + " HEAD\x00symref=HEAD:refs/heads/main\n"
			_, _ = fmt.Fprintf(response, "001e# service=git-upload-pack\n0000%04x%s0000", len(line)+4, line)
		case "/codeload/contoso/docs/tar.gz/" + testCommit:
			if archiveCalls.Add(1) == 1 {
				// First stream breaks mid-archive; the provider must restart.
				_, _ = response.Write(archive[:len(archive)/2])
				return
			}
			_, _ = response.Write(archive)
		default:
			http.NotFound(response, request)
		}
	}))
	defer server.Close()
	datasets, err := ParseCatalog([]byte(`datasets:
  - repo: contoso/docs
    name: Contoso docs
    description: Contoso identity documentation.
    include: ["docs/**/*.md", "README.md"]
    fragments: ["**/includes/**"]
    site_root: https://learn.example.com
    url_rules:
      - {prefix: "docs/", base: "https://learn.example.com/en-us/entra/"}
`))
	if err != nil {
		t.Fatal(err)
	}
	backend := NewWithCatalog(datasets, server.URL+"/api", server.URL+"/codeload", server.URL+"/git")
	ctx := context.Background()
	available, err := backend.Discover(ctx, "identity", false)
	if err != nil || len(available) != 1 || available[0].ID != "contoso-docs" || available[0].Provider != ProviderID || len(available[0].Variants) != 1 {
		t.Fatalf("discover = %#v, %v", available, err)
	}
	if !backend.Owns("contoso-docs") || backend.Owns("rfc") {
		t.Fatal("ownership is wrong")
	}
	release, err := backend.Latest(ctx, "contoso-docs", "")
	if err != nil || release.Fingerprint != testCommit || apiCalls.Load() != 1 || gitCalls.Load() != 1 {
		t.Fatalf("latest = %#v, %v (api=%d git=%d)", release, err, apiCalls.Load(), gitCalls.Load())
	}
	stage := t.TempDir()
	manifest, err := backend.Acquire(ctx, "contoso-docs", "markdown", release, stage, "", func(string, int64, int64, string, float64, string) {})
	if err != nil {
		t.Fatal(err)
	}
	if archiveCalls.Load() != 2 || manifest.DocumentCount != 3 || manifest.Fingerprint != testCommit || manifest.Site.SourceDocuments != 3 {
		t.Fatalf("manifest = %#v archive calls=%d", manifest, archiveCalls.Load())
	}
	if _, err := os.Stat(filepath.Join(stage, "raw", "docs", "identity", "media", "ca.png")); err == nil {
		t.Fatal("media file was stored")
	}
	// Re-acquiring the same commit into the same stage reuses the work.
	if _, err := backend.Acquire(ctx, "contoso-docs", "markdown", release, stage, "", func(string, int64, int64, string, float64, string) {}); err != nil || archiveCalls.Load() != 2 {
		t.Fatalf("resume re-downloaded: calls=%d err=%v", archiveCalls.Load(), err)
	}

	corpus, err := backend.OpenCorpus(stage, manifest)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = corpus.Close() }()
	var records []sourceprovider.Record
	if err := corpus.ScanBodies(ctx, "", sourceprovider.ScanOptions{}, func(record sourceprovider.Record, _ sourceprovider.ScanPosition) error {
		records = append(records, record)
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if len(records) != 3 {
		t.Fatalf("records = %d", len(records))
	}
	var plan sourceprovider.Record
	for _, record := range records {
		if record.Title == "Plan Conditional Access" {
			plan = record
		}
	}
	if plan.URL != "https://learn.example.com/en-us/entra/identity/conditional-access" || plan.Temporal.ModifiedAt == nil || plan.Temporal.ModifiedAt.Format("2006-01-02") != "2025-05-06" {
		t.Fatalf("plan record = %#v", plan)
	}
	mfaID := documentID("docs/identity/mfa.md")
	for _, want := range []string{"Premium P1", "knowledge-read://read?dataset=contoso-docs&id=" + mfaID + "&section=methods", "[rooted](knowledge-read://read?dataset=contoso-docs&id=" + mfaID + " ", "(https://learn.example.com/entra/fundamentals/whats-new)", "https://github.com/contoso/docs/blob/" + testCommit + "/docs/identity/media/ca.png"} {
		if !strings.Contains(plan.Body, want) {
			t.Fatalf("body missing %q:\n%s", want, plan.Body)
		}
	}
	if strings.Contains(plan.Body, "ms.date") || strings.Contains(plan.Body, "[!INCLUDE") {
		t.Fatalf("front matter or include marker leaked:\n%s", plan.Body)
	}

	count, err := knowledgeindex.BuildTitle(ctx, stage, manifest.Fingerprint, corpus, sourceprovider.ScanOptions{}, func(uint64, int64, int64) {})
	if err != nil || count != 3 {
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
	result, err := reader.Search(ctx, "Premium P1 license", model.SearchOptions{Limit: 5, Snippets: true}, true)
	if err != nil {
		t.Fatal(err)
	}
	if len(result.Hits) == 0 || result.Hits[0].ID != plan.ID || !strings.Contains(result.Hits[0].Snippet, "Premium") {
		t.Fatalf("search = %#v", result)
	}
	document, err := reader.Read(ctx, "", plan.ID, model.ReadOptions{MaxChars: 10_000, IncludeOutline: true})
	if err != nil {
		t.Fatal(err)
	}
	if document.Format != "markdown" || !strings.Contains(document.Content, "**Source:** https://learn.example.com/en-us/entra/identity/conditional-access") || !strings.Contains(document.Content, "**Updated:** 05/06/2025") || len(document.Sections) != 3 {
		t.Fatalf("document = %#v", document)
	}
	section, err := reader.Read(ctx, "", plan.ID, model.ReadOptions{Section: "report-only-mode"})
	if err != nil || section.SectionFound == nil || !*section.SectionFound || !strings.Contains(section.Content, "Evaluate policies") || strings.Contains(section.Content, "trusted IP") {
		t.Fatalf("section = %#v, %v", section, err)
	}
	source, err := corpus.Read(ctx, plan, model.ReadOptions{Format: "source"})
	if err != nil || !strings.HasPrefix(source.Content, "---\ntitle: Plan Conditional Access") {
		t.Fatalf("source = %#v, %v", source, err)
	}
	if _, err := corpus.Read(ctx, sourceprovider.Record{ID: documentID("docs/includes/license-note.md")}, model.ReadOptions{}); err == nil {
		t.Fatal("fragments must not be readable as documents")
	}
}
