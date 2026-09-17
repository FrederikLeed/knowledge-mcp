// Package webpages indexes curated lists of web pages. Each dataset is a
// YAML or JSON list file in the provider's configuration directory; the file
// name (without extension) is the dataset ID. Pages are fetched politely,
// stored as raw HTML, and converted to Markdown when indexed or read.
package webpages

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/lkarlslund/knowledge-mcp/internal/htmlmarkdown"
	"github.com/lkarlslund/knowledge-mcp/internal/markdowndoc"
	"github.com/lkarlslund/knowledge-mcp/internal/model"
	"github.com/lkarlslund/knowledge-mcp/internal/provider"
	"golang.org/x/net/html"
	"golang.org/x/net/html/charset"
	"gopkg.in/yaml.v3"
)

const (
	ProviderID = "webpages"
	variantID  = "html"

	// DefaultUserAgent identifies as a regular browser; several curated
	// sites reject generic HTTP client agents.
	DefaultUserAgent = "Mozilla/5.0 (X11; Linux x86_64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/128.0 Safari/537.36 knowledge-mcp"

	defaultConcurrency = 2
	maxConcurrency     = 4
	defaultDelay       = time.Second
	maxPageBytes       = 20 << 20
	fetchAttempts      = 3

	documentsFile = "documents.json"
	rawDirectory  = "raw"

	TypeHTML = "html"
	TypePDF  = "pdf"

	statusOK     = "ok"
	statusStale  = "stale"
	statusFailed = "failed"
)

var (
	idPattern   = regexp.MustCompile(`^[a-z0-9][a-z0-9._-]{0,127}$`)
	configNames = []string{".yaml", ".yml", ".json"}
	skipTokens  = []string{"breadcrumb", "share", "cookie", "consent", "verticalnav", "sidebar", "skip-link", "sr-only", "visually-hidden"}
)

// Page is one curated page.
type Page struct {
	Slug        string `yaml:"slug" json:"slug"`
	Title       string `yaml:"title" json:"title"`
	URL         string `yaml:"url" json:"url"`
	Type        string `yaml:"type" json:"type,omitempty"`
	PDFLinkText string `yaml:"pdf_link_text" json:"pdf_link_text,omitempty"`
}

// Config is one dataset list file.
type Config struct {
	Name        string   `yaml:"name" json:"name"`
	Description string   `yaml:"description" json:"description"`
	Project     string   `yaml:"project" json:"project,omitempty"`
	Language    string   `yaml:"language" json:"language,omitempty"`
	Topics      []string `yaml:"topics" json:"topics,omitempty"`
	UserAgent   string   `yaml:"user_agent" json:"user_agent,omitempty"`
	Delay       string   `yaml:"delay" json:"delay,omitempty"`
	Concurrency int      `yaml:"concurrency" json:"concurrency,omitempty"`
	Pages       []Page   `yaml:"pages" json:"pages"`
}

// ParseConfig parses and validates a dataset list (YAML or JSON).
func ParseConfig(data []byte) (Config, error) {
	var config Config
	if err := yaml.Unmarshal(data, &config); err != nil {
		return Config{}, fmt.Errorf("parse page list: %w", err)
	}
	if len(config.Pages) == 0 {
		return Config{}, errors.New("page list has no pages")
	}
	seen := make(map[string]bool, len(config.Pages))
	for index := range config.Pages {
		page := &config.Pages[index]
		page.Slug, page.URL, page.Title = strings.TrimSpace(page.Slug), strings.TrimSpace(page.URL), strings.TrimSpace(page.Title)
		page.Type = strings.ToLower(strings.TrimSpace(page.Type))
		if page.Type == "" {
			page.Type = TypeHTML
		}
		if page.Type != TypeHTML && page.Type != TypePDF {
			return Config{}, fmt.Errorf("page %q: type must be %q or %q", page.Slug, TypeHTML, TypePDF)
		}
		if !idPattern.MatchString(page.Slug) {
			return Config{}, fmt.Errorf("page %d: invalid slug %q", index, page.Slug)
		}
		if seen[page.Slug] {
			return Config{}, fmt.Errorf("duplicate page slug %q", page.Slug)
		}
		seen[page.Slug] = true
		parsed, err := url.Parse(page.URL)
		if err != nil || (parsed.Scheme != "http" && parsed.Scheme != "https") || parsed.Host == "" {
			return Config{}, fmt.Errorf("page %q: invalid URL %q", page.Slug, page.URL)
		}
	}
	if config.Concurrency <= 0 {
		config.Concurrency = defaultConcurrency
	}
	config.Concurrency = min(config.Concurrency, maxConcurrency)
	if config.Delay != "" {
		if _, err := time.ParseDuration(config.Delay); err != nil {
			return Config{}, fmt.Errorf("invalid delay %q: %w", config.Delay, err)
		}
	}
	if config.Language == "" {
		config.Language = "en"
	}
	if config.Project == "" {
		config.Project = "Curated web pages"
	}
	return config, nil
}

func (c Config) delay() time.Duration {
	if parsed, err := time.ParseDuration(c.Delay); err == nil && parsed >= 0 {
		return parsed
	}
	return defaultDelay
}

func (c Config) userAgent() string {
	if strings.TrimSpace(c.UserAgent) != "" {
		return c.UserAgent
	}
	return DefaultUserAgent
}

type WebPages struct {
	configDir    string
	installedDir string
	http         *http.Client
	now          func() time.Time
	sleep        func(context.Context, time.Duration) error
}

// New returns a provider reading dataset lists from configDir. installedDir
// is the store's provider dataset directory, used to keep ownership of
// installed datasets whose list file was removed.
func New(configDir, installedDir string) *WebPages {
	transport := http.DefaultTransport.(*http.Transport).Clone()
	transport.ResponseHeaderTimeout = 45 * time.Second
	return &WebPages{configDir: configDir, installedDir: installedDir, http: &http.Client{Transport: transport, Timeout: 2 * time.Minute}, now: time.Now, sleep: sleepContext}
}

func sleepContext(ctx context.Context, duration time.Duration) error {
	if duration <= 0 {
		return ctx.Err()
	}
	timer := time.NewTimer(duration)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}

func (*WebPages) ID() string { return ProviderID }

func (*WebPages) Backfill(context.Context, string, *model.Manifest) bool { return false }

func (p *WebPages) configPath(dataset string) (string, bool) {
	if !idPattern.MatchString(dataset) || p.configDir == "" {
		return "", false
	}
	for _, extension := range configNames {
		candidate := filepath.Join(p.configDir, dataset+extension)
		if info, err := os.Stat(candidate); err == nil && info.Mode().IsRegular() {
			return candidate, true
		}
	}
	return "", false
}

func (p *WebPages) Owns(collection string) bool {
	if _, ok := p.configPath(collection); ok {
		return true
	}
	if p.installedDir == "" || !idPattern.MatchString(collection) {
		return false
	}
	_, err := os.Stat(filepath.Join(p.installedDir, collection, "manifest.json"))
	return err == nil
}

func (p *WebPages) loadConfig(dataset string) (Config, error) {
	path, ok := p.configPath(dataset)
	if !ok {
		return Config{}, fmt.Errorf("no page list for webpages dataset %q in %s", dataset, p.configDir)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return Config{}, err
	}
	config, err := ParseConfig(data)
	if err != nil {
		return Config{}, fmt.Errorf("%s: %w", path, err)
	}
	if config.Name == "" {
		config.Name = dataset
	}
	if config.Description == "" {
		config.Description = fmt.Sprintf("%d curated web pages from %s.", len(config.Pages), filepath.Base(path))
	}
	return config, nil
}

func (p *WebPages) Discover(_ context.Context, filter string, _ bool) ([]model.AvailableDataset, error) {
	if p.configDir == "" {
		return nil, nil
	}
	entries, err := os.ReadDir(p.configDir)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	filter = strings.ToLower(strings.TrimSpace(filter))
	today := p.now().UTC().Format("20060102")
	var result []model.AvailableDataset
	var problems []error
	seen := map[string]bool{}
	for _, entry := range entries {
		extension := filepath.Ext(entry.Name())
		dataset := strings.TrimSuffix(entry.Name(), extension)
		if entry.IsDir() || !isConfigExtension(extension) || seen[dataset] || !idPattern.MatchString(dataset) {
			continue
		}
		seen[dataset] = true
		config, err := p.loadConfig(dataset)
		if err != nil {
			problems = append(problems, err)
			continue
		}
		haystack := strings.ToLower(strings.Join(append([]string{dataset, config.Name, config.Description, config.Project, "web pages"}, config.Topics...), " "))
		if filter != "" && !strings.Contains(haystack, filter) {
			continue
		}
		result = append(result, model.AvailableDataset{
			Provider: ProviderID, Variant: variantID, ID: dataset, DisplayName: config.Name, Description: config.Description,
			Project: config.Project, ContentType: "Curated web pages", Profile: profile(config), Language: language(config.Language),
			OnlineSourceURL: sourceSite(config), ReleaseDate: today, Available: true, PartCount: len(config.Pages),
			Variants: []model.Variant{{ID: variantID, Name: "Web pages", Description: "Fetched HTML converted to Markdown; PDF entries are indexed by title and link only", Format: "text/html"}},
		})
	}
	if len(result) == 0 && len(problems) > 0 {
		return nil, errors.Join(problems...)
	}
	return result, nil
}

// ListSource describes one page list file for source management.
type ListSource struct {
	ID     string
	File   string
	Config Config
	Err    error
}

// Sources returns every page list file, including ones that fail validation.
func (p *WebPages) Sources() ([]ListSource, error) {
	if p.configDir == "" {
		return nil, nil
	}
	entries, err := os.ReadDir(p.configDir)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var result []ListSource
	seen := map[string]bool{}
	for _, entry := range entries {
		extension := filepath.Ext(entry.Name())
		dataset := strings.TrimSuffix(entry.Name(), extension)
		if entry.IsDir() || !isConfigExtension(extension) || seen[dataset] || !idPattern.MatchString(dataset) {
			continue
		}
		seen[dataset] = true
		source := ListSource{ID: dataset}
		if path, ok := p.configPath(dataset); ok {
			source.File = filepath.Base(path)
		}
		source.Config, source.Err = p.loadConfig(dataset)
		result = append(result, source)
	}
	return result, nil
}

// ReadSource returns the raw page list file for dataset.
func (p *WebPages) ReadSource(dataset string) (string, error) {
	path, ok := p.configPath(dataset)
	if !ok {
		return "", fmt.Errorf("no page list named %q", dataset)
	}
	data, err := os.ReadFile(path)
	return string(data), err
}

// SaveSource validates content and atomically writes the page list for
// dataset, keeping an existing file's extension and defaulting to .yaml.
func (p *WebPages) SaveSource(dataset, content string) error {
	if p.configDir == "" {
		return errors.New("no webpages directory is configured")
	}
	if !idPattern.MatchString(dataset) {
		return fmt.Errorf("invalid page list name %q: use lowercase letters, digits, '.', '_' or '-'", dataset)
	}
	if _, err := ParseConfig([]byte(content)); err != nil {
		return err
	}
	path, ok := p.configPath(dataset)
	if !ok {
		path = filepath.Join(p.configDir, dataset+".yaml")
	}
	return provider.WriteFileAtomic(path, []byte(content))
}

// DeleteSource removes the page list file for dataset. Installed data stays
// until the dataset itself is deleted.
func (p *WebPages) DeleteSource(dataset string) error {
	path, ok := p.configPath(dataset)
	if !ok {
		return fmt.Errorf("no page list named %q", dataset)
	}
	return os.Remove(path)
}

func isConfigExtension(extension string) bool {
	for _, candidate := range configNames {
		if strings.EqualFold(candidate, extension) {
			return true
		}
	}
	return false
}

func sourceSite(config Config) string {
	if parsed, err := url.Parse(config.Pages[0].URL); err == nil {
		return parsed.Scheme + "://" + parsed.Host + "/"
	}
	return ""
}

func profile(config Config) model.DatasetProfile {
	return model.DatasetProfile{Topics: config.Topics, DocumentTypes: []string{"web pages", "regulations", "guidance"}, UpdateCadence: "Refetched at most weekly", CoverageNotes: fmt.Sprintf("%d curated pages; PDF documents are listed by title and link without extracted text.", len(config.Pages)), SourceFeatures: []string{"main-content extraction", "Markdown tables", "absolute source links"}}
}

func language(code string) model.Language {
	switch code {
	case "da":
		return model.Language{Code: "da", Name: "Danish", LocalName: "Dansk", Direction: "ltr"}
	default:
		return model.Language{Code: "en", Name: "English", LocalName: "English", Direction: "ltr"}
	}
}

type release struct {
	Config Config
}

// Latest fingerprints the page list together with the ISO week, so a list
// edit or a new week produces a new release that refetches every page.
func (p *WebPages) Latest(_ context.Context, collection, variant string) (provider.Release, error) {
	if variant != "" && variant != variantID {
		return provider.Release{}, fmt.Errorf("unknown webpages variant %q", variant)
	}
	config, err := p.loadConfig(collection)
	if err != nil {
		return provider.Release{}, err
	}
	now := p.now().UTC()
	year, week := now.ISOWeek()
	canonical, err := json.Marshal(config.Pages)
	if err != nil {
		return provider.Release{}, err
	}
	sum := sha256.Sum256(append(canonical, []byte(fmt.Sprintf("|%04d-W%02d", year, week))...))
	return provider.Release{Fingerprint: hex.EncodeToString(sum[:]), Date: now.Format("20060102"), Value: &release{Config: config}}, nil
}

type pageEntry struct {
	Page
	Status      string `json:"status"`
	File        string `json:"file,omitempty"`
	ResolvedURL string `json:"resolved_url,omitempty"`
	ContentType string `json:"content_type,omitempty"`
	FetchedAt   string `json:"fetched_at,omitempty"`
	Error       string `json:"error,omitempty"`
}

type documentIndex struct {
	Dataset string      `json:"dataset"`
	Config  Config      `json:"config"`
	Entries []pageEntry `json:"entries"`
}

func (p *WebPages) Acquire(ctx context.Context, collection, _ string, value provider.Release, stage, current string, progress provider.Progress) (model.Manifest, error) {
	resolved, ok := value.Value.(*release)
	if !ok {
		config, err := p.loadConfig(collection)
		if err != nil {
			return model.Manifest{}, err
		}
		resolved = &release{Config: config}
	}
	config := resolved.Config
	rawDir := filepath.Join(stage, rawDirectory)
	if err := os.MkdirAll(rawDir, 0o755); err != nil {
		return model.Manifest{}, err
	}
	previous := map[string]pageEntry{}
	if current != "" {
		if index, err := readIndex(current); err == nil {
			for _, entry := range index.Entries {
				previous[entry.Slug] = entry
			}
		}
	}
	// Pages already fetched into this stage (an interrupted job) are kept.
	resumed := map[string]pageEntry{}
	if index, err := readIndex(stage); err == nil {
		for _, entry := range index.Entries {
			if entry.Status == statusOK {
				if _, statErr := os.Stat(filepath.Join(rawDir, entry.File)); statErr == nil {
					resumed[entry.Slug] = entry
				}
			}
		}
	}
	entries := make([]pageEntry, len(config.Pages))
	var mu sync.Mutex
	var completed, failed int64
	checkpoint := func() error {
		data, err := json.MarshalIndent(documentIndex{Dataset: collection, Config: config, Entries: entries}, "", "  ")
		if err != nil {
			return err
		}
		return writeAtomic(filepath.Join(stage, documentsFile), data)
	}
	tasks := make(chan int)
	var wg sync.WaitGroup
	for range min(config.Concurrency, len(config.Pages)) {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for index := range tasks {
				page := config.Pages[index]
				entry, fetched := resumed[page.Slug]
				if !fetched || entry.URL != page.URL || entry.Type != page.Type {
					entry = p.fetchPage(ctx, config, page, rawDir)
					if entry.Status == statusFailed {
						entry = reusePrevious(entry, previous[page.Slug], current, rawDir)
					}
					if ctx.Err() == nil {
						_ = p.sleep(ctx, config.delay())
					}
				}
				mu.Lock()
				entries[index] = entry
				completed++
				if entry.Status != statusOK {
					failed++
				}
				if completed%10 == 0 {
					_ = checkpoint()
				}
				progress("downloading_pages", completed, int64(len(config.Pages)), "pages", 0, fmt.Sprintf("fetching pages (%d not fetched)", failed))
				mu.Unlock()
			}
		}()
	}
send:
	for index := range config.Pages {
		select {
		case tasks <- index:
		case <-ctx.Done():
			break send
		}
	}
	close(tasks)
	wg.Wait()
	if err := ctx.Err(); err != nil {
		mu.Lock()
		_ = checkpoint()
		mu.Unlock()
		return model.Manifest{}, err
	}
	if err := checkpoint(); err != nil {
		return model.Manifest{}, err
	}
	var rawBytes int64
	documents := 0
	var failures []string
	for _, entry := range entries {
		switch entry.Status {
		case statusOK, statusStale:
			documents++
			if info, err := os.Stat(filepath.Join(rawDir, entry.File)); err == nil {
				rawBytes += info.Size()
			}
		default:
			failures = append(failures, entry.Slug+": "+entry.Error)
		}
	}
	if documents == 0 {
		return model.Manifest{}, fmt.Errorf("no pages could be fetched: %s", strings.Join(failures, "; "))
	}
	if len(failures) > 0 {
		progress("downloading_pages", completed, int64(len(config.Pages)), "pages", 0, fmt.Sprintf("%d of %d pages unavailable; see %s", len(failures), len(config.Pages), documentsFile))
	}
	var metadataBytes int64
	if info, err := os.Stat(filepath.Join(stage, documentsFile)); err == nil {
		metadataBytes = info.Size()
	}
	now := p.now().UTC()
	return model.Manifest{
		Provider: ProviderID, Variant: variantID, Dataset: collection, ReleaseDate: value.Date, Fingerprint: value.Fingerprint,
		PartCount: len(config.Pages), RawSize: rawBytes, ProviderMetadataSize: metadataBytes, DocumentCount: uint64(documents), PublishedAt: now,
		Site: model.DatasetMetadata{Name: config.Name, Description: config.Description, Project: config.Project, ContentType: "Curated web pages", Profile: profile(config), Language: language(config.Language), OnlineSourceURL: sourceSite(config), SourceDocuments: uint64(documents), MetadataUpdatedAt: now},
	}, nil
}

// reusePrevious keeps the last good copy of a page that failed to refetch.
func reusePrevious(failed, previous pageEntry, current, rawDir string) pageEntry {
	if current == "" || previous.File == "" || (previous.Status != statusOK && previous.Status != statusStale) || previous.URL != failed.URL {
		return failed
	}
	if err := linkOrCopy(filepath.Join(current, rawDirectory, previous.File), filepath.Join(rawDir, previous.File)); err != nil {
		return failed
	}
	previous.Status, previous.Error = statusStale, failed.Error
	return previous
}

func (p *WebPages) fetchPage(ctx context.Context, config Config, page Page, rawDir string) pageEntry {
	entry := pageEntry{Page: page, FetchedAt: p.now().UTC().Format(time.RFC3339)}
	fail := func(err error) pageEntry {
		entry.Status, entry.Error = statusFailed, err.Error()
		return entry
	}
	if page.Type == TypePDF {
		target := page.URL
		if !strings.HasSuffix(strings.ToLower(pathOf(page.URL)), ".pdf") && page.PDFLinkText != "" {
			body, _, _, err := p.get(ctx, config, page.URL)
			if err != nil {
				return fail(err)
			}
			link, err := findPDFLink(body, page.URL, page.PDFLinkText)
			if err != nil {
				return fail(err)
			}
			target = link
		}
		entry.ResolvedURL, entry.ContentType = target, "application/pdf"
		entry.File = page.Slug + ".pdf.json"
		data, _ := json.Marshal(map[string]string{"url": page.URL, "resolved_url": target})
		if err := writeAtomic(filepath.Join(rawDir, entry.File), data); err != nil {
			return fail(err)
		}
		entry.Status = statusOK
		return entry
	}
	body, contentType, finalURL, err := p.get(ctx, config, page.URL)
	if err != nil {
		return fail(err)
	}
	entry.ContentType = contentType
	if finalURL != page.URL {
		entry.ResolvedURL = finalURL
	}
	if strings.Contains(strings.ToLower(contentType), "pdf") {
		entry.Type, entry.ResolvedURL, entry.File = TypePDF, finalURL, page.Slug+".pdf.json"
		data, _ := json.Marshal(map[string]string{"url": page.URL, "resolved_url": finalURL})
		body = data
	} else {
		entry.File = page.Slug + ".html"
		if decoded, decodeErr := charset.NewReader(bytes.NewReader(body), contentType); decodeErr == nil {
			if utf8Body, readErr := io.ReadAll(decoded); readErr == nil {
				body = utf8Body
			}
		}
	}
	if err := writeAtomic(filepath.Join(rawDir, entry.File), body); err != nil {
		return fail(err)
	}
	entry.Status = statusOK
	return entry
}

func pathOf(raw string) string {
	if parsed, err := url.Parse(raw); err == nil {
		return parsed.Path
	}
	return raw
}

type statusError struct {
	status     int
	retryAfter string
}

func (e *statusError) Error() string { return "HTTP " + strconv.Itoa(e.status) }

// get fetches a URL with retries for transient failures.
func (p *WebPages) get(ctx context.Context, config Config, target string) ([]byte, string, string, error) {
	var lastErr error
	for attempt := 1; attempt <= fetchAttempts; attempt++ {
		body, contentType, finalURL, err := p.getOnce(ctx, config, target)
		if err == nil {
			return body, contentType, finalURL, nil
		}
		lastErr = err
		var status *statusError
		if errors.As(err, &status) && status.status != http.StatusTooManyRequests && status.status < 500 {
			break
		}
		if attempt == fetchAttempts || ctx.Err() != nil {
			break
		}
		wait := time.Duration(attempt*attempt) * 2 * time.Second
		if status != nil {
			if seconds, parseErr := strconv.Atoi(strings.TrimSpace(status.retryAfter)); parseErr == nil && seconds >= 0 && seconds <= 120 {
				wait = time.Duration(seconds) * time.Second
			}
		}
		if err := p.sleep(ctx, wait); err != nil {
			return nil, "", "", err
		}
	}
	return nil, "", "", fmt.Errorf("fetch %s: %w", target, lastErr)
}

func (p *WebPages) getOnce(ctx context.Context, config Config, target string) ([]byte, string, string, error) {
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, target, nil)
	if err != nil {
		return nil, "", "", err
	}
	request.Header.Set("User-Agent", config.userAgent())
	request.Header.Set("Accept", "text/html,application/xhtml+xml,application/pdf;q=0.9,*/*;q=0.8")
	request.Header.Set("Accept-Language", config.Language+",en;q=0.8")
	response, err := p.http.Do(request)
	if err != nil {
		return nil, "", "", err
	}
	defer func() { _ = response.Body.Close() }()
	if response.StatusCode != http.StatusOK {
		_, _ = io.Copy(io.Discard, io.LimitReader(response.Body, 1<<16))
		return nil, "", "", &statusError{status: response.StatusCode, retryAfter: response.Header.Get("Retry-After")}
	}
	contentType := response.Header.Get("Content-Type")
	finalURL := response.Request.URL.String()
	if strings.Contains(strings.ToLower(contentType), "pdf") {
		// PDF text extraction is not supported; the link is what gets indexed.
		return nil, contentType, finalURL, nil
	}
	body, err := io.ReadAll(io.LimitReader(response.Body, maxPageBytes+1))
	if err != nil {
		return nil, "", "", err
	}
	if len(body) > maxPageBytes {
		return nil, "", "", fmt.Errorf("page exceeds %d bytes", maxPageBytes)
	}
	return body, contentType, finalURL, nil
}

// findPDFLink resolves a PDF asset link on a landing page: first an anchor
// whose text contains the wanted text, otherwise the first asset link after
// the heading or element id that names the document.
func findPDFLink(source []byte, pageURL, want string) (string, error) {
	document, err := html.Parse(bytes.NewReader(source))
	if err != nil {
		return "", err
	}
	want = strings.ToLower(strings.TrimSpace(want))
	base, _ := url.Parse(pageURL)
	var nodes []*html.Node
	var walk func(*html.Node)
	walk = func(node *html.Node) {
		if node.Type == html.ElementNode {
			nodes = append(nodes, node)
		}
		for child := node.FirstChild; child != nil; child = child.NextSibling {
			walk(child)
		}
	}
	walk(document)
	resolve := func(href string) string {
		reference, err := url.Parse(strings.TrimSpace(href))
		if err != nil || base == nil {
			return href
		}
		return base.ResolveReference(reference).String()
	}
	isAsset := func(href string) bool {
		lower := strings.ToLower(href)
		return strings.Contains(lower, ".pdf") || strings.Contains(lower, "/assets/") || strings.Contains(lower, "/media/")
	}
	for _, node := range nodes {
		if node.Data == "a" && htmlmarkdown.Attribute(node, "href") != "" && strings.Contains(strings.ToLower(htmlmarkdown.NodeText(node)), want) {
			return resolve(htmlmarkdown.Attribute(node, "href")), nil
		}
	}
	for index, node := range nodes {
		id := strings.ReplaceAll(strings.ToLower(htmlmarkdown.Attribute(node, "id")), "-", " ")
		heading := (node.Data == "h2" || node.Data == "h3" || node.Data == "h4") && strings.Contains(strings.ToLower(htmlmarkdown.NodeText(node)), want)
		if !strings.Contains(id, want) && !heading {
			continue
		}
		for _, candidate := range nodes[index+1:] {
			if href := htmlmarkdown.Attribute(candidate, "href"); candidate.Data == "a" && isAsset(href) {
				return resolve(href), nil
			}
		}
	}
	return "", fmt.Errorf("PDF link %q not found on %s", want, pageURL)
}

// corpus serves one installed generation.
type corpus struct {
	path    string
	dataset string
	entries []pageEntry
	bySlug  map[string]int
}

func (p *WebPages) OpenCorpus(directory string, manifest model.Manifest) (provider.Corpus, error) {
	index, err := readIndex(directory)
	if err != nil {
		return nil, err
	}
	c := &corpus{path: directory, dataset: manifest.Dataset, bySlug: map[string]int{}}
	for _, entry := range index.Entries {
		if entry.Status == statusOK || entry.Status == statusStale {
			c.bySlug[entry.Slug] = len(c.entries)
			c.entries = append(c.entries, entry)
		}
	}
	return c, nil
}

func (*corpus) Close() error { return nil }

func (c *corpus) ScanTitles(ctx context.Context, after string, _ provider.ScanOptions, sink provider.RecordSink) error {
	return c.scan(ctx, after, false, sink)
}

func (c *corpus) ScanBodies(ctx context.Context, after string, _ provider.ScanOptions, sink provider.RecordSink) error {
	return c.scan(ctx, after, true, sink)
}

func (c *corpus) scan(ctx context.Context, after string, bodies bool, sink provider.RecordSink) error {
	start := 0
	if after != "" {
		parsed, err := strconv.Atoi(after)
		if err != nil || parsed < 0 || parsed > len(c.entries) {
			return fmt.Errorf("invalid webpages scan cursor %q", after)
		}
		start = parsed
	}
	for index := start; index < len(c.entries); index++ {
		if err := ctx.Err(); err != nil {
			return err
		}
		entry := c.entries[index]
		body, title := "", entry.Title
		if bodies || title == "" {
			var err error
			if body, title, err = c.render(entry); err != nil {
				return err
			}
		}
		record := provider.Record{ID: entry.Slug, Title: title, URL: entry.URL, Locator: entry.Slug, Primary: true, Identifiers: []string{entry.Slug}, Keywords: []string{c.dataset}, RankWeight: 1, Metadata: map[string]string{"type": entry.Type, "fetched_at": entry.FetchedAt}}
		if entry.Type == TypePDF {
			record.RankWeight = 0.6
			record.Keywords = append(record.Keywords, "PDF")
		}
		if bodies {
			record.Body = body
		}
		if err := sink(record, provider.ScanPosition{Cursor: strconv.Itoa(index + 1), Completed: int64(index + 1), Total: int64(len(c.entries)), Units: "pages", Boundary: true}); err != nil {
			return err
		}
	}
	return nil
}

// render returns the Markdown body and effective title of a page.
func (c *corpus) render(entry pageEntry) (string, string, error) {
	data, err := os.ReadFile(filepath.Join(c.path, rawDirectory, entry.File))
	if errors.Is(err, os.ErrNotExist) {
		return "", "", provider.ErrDocumentNotFound
	}
	if err != nil {
		return "", "", err
	}
	if entry.Type == TypePDF {
		target := firstNonEmpty(entry.ResolvedURL, entry.URL)
		title := firstNonEmpty(entry.Title, entry.Slug)
		return fmt.Sprintf("# %s\n\nPDF document: [%s](%s)\n\nThe PDF text is not indexed; open the link to read the document. Landing page: %s\n", title, title, target, entry.URL), title, nil
	}
	pageURL := firstNonEmpty(entry.ResolvedURL, entry.URL)
	body, pageTitle := PageMarkdown(data, pageURL)
	return body, firstNonEmpty(entry.Title, pageTitle, entry.Slug), nil
}

func firstNonEmpty(values ...string) string {
	for _, value := range values {
		if value = strings.TrimSpace(value); value != "" {
			return value
		}
	}
	return ""
}

func (c *corpus) Read(_ context.Context, record provider.Record, options model.ReadOptions) (model.Document, error) {
	position, ok := c.bySlug[record.ID]
	if !ok {
		return model.Document{}, provider.ErrDocumentNotFound
	}
	entry := c.entries[position]
	body, title, err := c.render(entry)
	if err != nil {
		return model.Document{}, err
	}
	format := "markdown"
	content := body
	switch options.Format {
	case "source":
		format = "source"
		data, readErr := os.ReadFile(filepath.Join(c.path, rawDirectory, entry.File))
		if readErr != nil {
			return model.Document{}, readErr
		}
		content = string(data)
	case "text":
		format = "text"
	default:
		var header strings.Builder
		fmt.Fprintf(&header, "**Source:** %s  \n**Fetched:** %s", entry.URL, entry.FetchedAt)
		if entry.Status == statusStale {
			header.WriteString(" (latest refetch failed; showing the previous copy)")
		}
		header.WriteString("\n\n")
		if markdowndoc.FirstHeading(body) == "" {
			content = "# " + title + "\n\n" + header.String() + body
		} else {
			content = header.String() + body
		}
	}
	document := model.Document{TemporalMetadata: record.Temporal, ID: entry.Slug, Title: title, URL: entry.URL, Format: format}
	return markdowndoc.Page(document, content, options), nil
}

// PageMarkdown extracts the main content of an HTML page as Markdown with
// absolute links, and returns the page's own title.
func PageMarkdown(source []byte, pageURL string) (string, string) {
	document, err := html.Parse(bytes.NewReader(source))
	if err != nil {
		return strings.TrimSpace(string(source)), ""
	}
	root := mainContent(document)
	title := ""
	isH1 := func(node *html.Node) bool { return node.Data == "h1" }
	if h1 := findFirst(root, isH1); h1 != nil {
		title = htmlmarkdown.NodeText(h1)
	} else if h1 := findFirst(document, isH1); h1 != nil {
		title = htmlmarkdown.NodeText(h1)
	} else if element := findFirst(document, func(node *html.Node) bool { return node.Data == "title" }); element != nil {
		title = htmlmarkdown.NodeText(element)
	}
	base, _ := url.Parse(pageURL)
	body := htmlmarkdown.ConvertNode(root, htmlmarkdown.Options{
		Link: func(href string) string {
			href = strings.TrimSpace(href)
			if href == "" || strings.HasPrefix(href, "#") || base == nil {
				return href
			}
			reference, err := url.Parse(href)
			if err != nil {
				return href
			}
			return base.ResolveReference(reference).String()
		},
		Skip: skipNode,
	})
	return body, title
}

func mainContent(document *html.Node) *html.Node {
	matchers := []func(*html.Node) bool{
		func(node *html.Node) bool { return node.Data == "main" },
		func(node *html.Node) bool { return strings.EqualFold(htmlmarkdown.Attribute(node, "role"), "main") },
		func(node *html.Node) bool { return node.Data == "article" },
		func(node *html.Node) bool {
			id := strings.ToLower(htmlmarkdown.Attribute(node, "id"))
			return id == "content" || id == "main" || id == "main-content"
		},
		func(node *html.Node) bool { return node.Data == "body" },
	}
	for _, matcher := range matchers {
		if node := findFirst(document, matcher); node != nil {
			return node
		}
	}
	return document
}

func findFirst(node *html.Node, match func(*html.Node) bool) *html.Node {
	if node.Type == html.ElementNode && match(node) {
		return node
	}
	for child := node.FirstChild; child != nil; child = child.NextSibling {
		if found := findFirst(child, match); found != nil {
			return found
		}
	}
	return nil
}

func skipNode(node *html.Node) bool {
	switch node.Data {
	case "nav", "aside", "form", "button", "iframe", "template", "dialog", "select", "input", "footer", "head":
		return true
	}
	identity := strings.ToLower(htmlmarkdown.Attribute(node, "class") + " " + htmlmarkdown.Attribute(node, "id"))
	if strings.TrimSpace(identity) == "" {
		return false
	}
	for _, token := range skipTokens {
		if strings.Contains(identity, token) {
			return true
		}
	}
	return false
}

func readIndex(directory string) (documentIndex, error) {
	data, err := os.ReadFile(filepath.Join(directory, documentsFile))
	if err != nil {
		return documentIndex{}, err
	}
	var index documentIndex
	err = json.Unmarshal(data, &index)
	return index, err
}

func writeAtomic(destination string, data []byte) error {
	temporary := destination + ".partial"
	if err := os.WriteFile(temporary, data, 0o644); err != nil {
		return err
	}
	return os.Rename(temporary, destination)
}

func linkOrCopy(source, destination string) error {
	if err := os.Link(source, destination); err == nil {
		return nil
	}
	data, err := os.ReadFile(source)
	if err != nil {
		return err
	}
	return writeAtomic(destination, data)
}
