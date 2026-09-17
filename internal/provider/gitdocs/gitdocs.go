// Package gitdocs indexes Markdown documentation stored in GitHub
// repositories. Each catalog entry is one dataset. Acquisition streams the
// repository tarball for one commit and keeps only matching text files, so
// repositories with large media trees never touch the disk in full.
package gitdocs

import (
	"archive/tar"
	"context"
	"crypto/sha256"
	_ "embed"
	"encoding/base32"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/klauspost/compress/gzip"
	"github.com/lkarlslund/knowledge-mcp/internal/model"
	"github.com/lkarlslund/knowledge-mcp/internal/provider"
	"gopkg.in/yaml.v3"
)

const (
	ProviderID = "gitdocs"
	variantID  = "markdown"
	userAgent  = "knowledge-mcp (+https://github.com/lkarlslund/knowledge-mcp)"

	// maxTextFile bounds a single kept file; larger "documentation" files are
	// generated artifacts, not prose.
	maxTextFile = 4 << 20
	// maxAcquireAttempts bounds full tarball restarts after a broken stream.
	maxAcquireAttempts = 3

	documentsFile = "documents.json"
	rawDirectory  = "raw"
)

//go:embed catalog.yaml
var builtinCatalog []byte

var (
	datasetIDPattern = regexp.MustCompile(`^[a-z0-9][a-z0-9._-]{0,127}$`)
	repoPattern      = regexp.MustCompile(`^[A-Za-z0-9_.-]+/[A-Za-z0-9_.-]+$`)
	shaPattern       = regexp.MustCompile(`^[0-9a-f]{40}$`)
)

// URLRule maps a repository path prefix to a published documentation URL.
type URLRule struct {
	Prefix        string `yaml:"prefix"`
	Base          string `yaml:"base"`
	Query         string `yaml:"query"`
	Lowercase     bool   `yaml:"lowercase"`
	TrailingSlash bool   `yaml:"trailing_slash"`
}

// Dataset is one catalog entry.
type Dataset struct {
	ID          string    `yaml:"id"`
	Repo        string    `yaml:"repo"`
	Branch      string    `yaml:"branch"`
	Name        string    `yaml:"name"`
	Description string    `yaml:"description"`
	Project     string    `yaml:"project"`
	Language    string    `yaml:"language"`
	Topics      []string  `yaml:"topics"`
	Include     []string  `yaml:"include"`
	Exclude     []string  `yaml:"exclude"`
	Fragments   []string  `yaml:"fragments"`
	SiteRoot    string    `yaml:"site_root"`
	URLRules    []URLRule `yaml:"url_rules"`
	// Custom marks entries loaded from the editable custom catalog file.
	Custom bool `yaml:"-"`
}

// ParseCatalog parses and validates a catalog document.
func ParseCatalog(data []byte) ([]Dataset, error) {
	var file struct {
		Datasets []Dataset `yaml:"datasets"`
	}
	if err := yaml.Unmarshal(data, &file); err != nil {
		return nil, fmt.Errorf("parse gitdocs catalog: %w", err)
	}
	seen := map[string]bool{}
	for index := range file.Datasets {
		item := &file.Datasets[index]
		if !repoPattern.MatchString(item.Repo) {
			return nil, fmt.Errorf("gitdocs catalog entry %d: invalid repo %q", index, item.Repo)
		}
		if item.ID == "" {
			item.ID = strings.ToLower(strings.ReplaceAll(item.Repo, "/", "-"))
		}
		if !datasetIDPattern.MatchString(item.ID) {
			return nil, fmt.Errorf("gitdocs catalog entry %q: invalid dataset ID %q", item.Repo, item.ID)
		}
		if seen[item.ID] {
			return nil, fmt.Errorf("gitdocs catalog: duplicate dataset ID %q", item.ID)
		}
		seen[item.ID] = true
		if len(item.Include) == 0 {
			return nil, fmt.Errorf("gitdocs catalog entry %q: include globs are required", item.ID)
		}
		if item.Name == "" {
			item.Name = item.Repo
		}
		if item.Description == "" {
			item.Description = "Markdown documentation from the GitHub repository " + item.Repo + "."
		}
		if item.Project == "" {
			item.Project = "GitHub"
		}
		if item.Language == "" {
			item.Language = "en"
		}
		item.SiteRoot = strings.TrimRight(item.SiteRoot, "/")
	}
	return file.Datasets, nil
}

type GitDocs struct {
	builtin      []Dataset
	customPath   string
	mu           sync.RWMutex
	datasets     []Dataset
	byID         map[string]*Dataset
	customStamp  string
	customErr    error
	apiBase      string
	codeloadBase string
	gitBase      string
	token        string
	http         *http.Client
}

// New returns the provider with the built-in catalog plus the optional custom
// catalog at customPath, which is re-read whenever the file changes.
func New(customPath string) *GitDocs {
	datasets, err := ParseCatalog(builtinCatalog)
	if err != nil {
		panic(err)
	}
	p := NewWithCatalog(datasets, "https://api.github.com", "https://codeload.github.com", "https://github.com")
	p.customPath = customPath
	p.reload()
	return p
}

// NewWithCatalog returns a provider for explicit datasets and GitHub endpoints.
func NewWithCatalog(datasets []Dataset, apiBase, codeloadBase, gitBase string) *GitDocs {
	transport := http.DefaultTransport.(*http.Transport).Clone()
	transport.ResponseHeaderTimeout = 60 * time.Second
	p := &GitDocs{builtin: datasets, apiBase: strings.TrimRight(apiBase, "/"), codeloadBase: strings.TrimRight(codeloadBase, "/"), gitBase: strings.TrimRight(gitBase, "/"), token: strings.TrimSpace(os.Getenv("GITHUB_TOKEN")), http: &http.Client{Transport: transport}}
	p.setDatasets(datasets)
	return p
}

func (p *GitDocs) setDatasets(datasets []Dataset) {
	byID := make(map[string]*Dataset, len(datasets))
	for index := range datasets {
		byID[datasets[index].ID] = &datasets[index]
	}
	p.datasets, p.byID = datasets, byID
}

// MergeCustomCatalog validates a custom catalog document against the built-in
// entries and returns the combined catalog.
func MergeCustomCatalog(builtin []Dataset, data []byte) ([]Dataset, error) {
	merged := append([]Dataset(nil), builtin...)
	if len(strings.TrimSpace(string(data))) == 0 {
		return merged, nil
	}
	custom, err := ParseCatalog(data)
	if err != nil {
		return nil, err
	}
	for _, item := range custom {
		for _, existing := range builtin {
			if existing.ID == item.ID {
				return nil, fmt.Errorf("custom gitdocs entry %q duplicates a built-in dataset ID", item.ID)
			}
		}
		item.Custom = true
		merged = append(merged, item)
	}
	return merged, nil
}

func customStamp(info os.FileInfo) string {
	return fmt.Sprintf("%d:%d", info.ModTime().UnixNano(), info.Size())
}

// reload re-reads the custom catalog when its file changed. A broken file
// keeps the built-in catalog available and is reported by CustomCatalog.
func (p *GitDocs) reload() {
	if p.customPath == "" {
		return
	}
	stamp := ""
	info, statErr := os.Stat(p.customPath)
	if statErr == nil {
		stamp = customStamp(info)
	}
	p.mu.RLock()
	unchanged := stamp == p.customStamp
	p.mu.RUnlock()
	if unchanged {
		return
	}
	var data []byte
	var err error
	if statErr == nil {
		data, err = os.ReadFile(p.customPath)
	} else if !errors.Is(statErr, os.ErrNotExist) {
		err = statErr
	}
	datasets := p.builtin
	if err == nil {
		datasets, err = MergeCustomCatalog(p.builtin, data)
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	p.customStamp, p.customErr = stamp, err
	if err != nil {
		datasets = p.builtin
	}
	p.setDatasets(datasets)
}

func (p *GitDocs) lookup(id string) *Dataset {
	p.reload()
	p.mu.RLock()
	defer p.mu.RUnlock()
	return p.byID[id]
}

// Catalog returns every dataset, built-in entries first.
func (p *GitDocs) Catalog() []Dataset {
	p.reload()
	p.mu.RLock()
	defer p.mu.RUnlock()
	return append([]Dataset(nil), p.datasets...)
}

// CustomCatalog returns the raw custom catalog file and the error, if any,
// that kept it from loading.
func (p *GitDocs) CustomCatalog() (string, error) {
	p.reload()
	if p.customPath == "" {
		return "", errors.New("no custom gitdocs catalog is configured")
	}
	data, err := os.ReadFile(p.customPath)
	if errors.Is(err, os.ErrNotExist) {
		return "", nil
	}
	if err != nil {
		return "", err
	}
	p.mu.RLock()
	defer p.mu.RUnlock()
	return string(data), p.customErr
}

// SaveCustomCatalog validates and atomically replaces the custom catalog.
func (p *GitDocs) SaveCustomCatalog(content string) error {
	if p.customPath == "" {
		return errors.New("no custom gitdocs catalog is configured")
	}
	if _, err := MergeCustomCatalog(p.builtin, []byte(content)); err != nil {
		return err
	}
	if err := provider.WriteFileAtomic(p.customPath, []byte(content)); err != nil {
		return err
	}
	p.reload()
	return nil
}

func (*GitDocs) ID() string { return ProviderID }

func (p *GitDocs) Owns(collection string) bool { return p.lookup(collection) != nil }

func (*GitDocs) Backfill(context.Context, string, *model.Manifest) bool { return false }

func (p *GitDocs) Discover(_ context.Context, filter string, _ bool) ([]model.AvailableDataset, error) {
	filter = strings.ToLower(strings.TrimSpace(filter))
	today := time.Now().UTC().Format("20060102")
	var result []model.AvailableDataset
	for _, item := range p.Catalog() {
		haystack := strings.ToLower(strings.Join(append([]string{item.ID, item.Repo, item.Name, item.Description, item.Project, "github markdown documentation gitdocs"}, item.Topics...), " "))
		if filter != "" && !strings.Contains(haystack, filter) {
			continue
		}
		result = append(result, model.AvailableDataset{
			Provider: ProviderID, Variant: variantID, ID: item.ID, DisplayName: item.Name, Description: item.Description,
			Project: item.Project, ContentType: "Technical documentation", Profile: profile(item), Language: language(item.Language),
			OnlineSourceURL: "https://github.com/" + item.Repo, ReleaseDate: today, Available: true, PartCount: 1,
			Variants: []model.Variant{variant()},
		})
	}
	return result, nil
}

func variant() model.Variant {
	return model.Variant{ID: variantID, Name: "Markdown text", Description: "Markdown and DocFX YAML text files at the latest commit; media files are never downloaded", Format: "text/markdown"}
}

func profile(item Dataset) model.DatasetProfile {
	return model.DatasetProfile{Topics: item.Topics, DocumentTypes: []string{"documentation", "how-to guides", "reference"}, UpdateCadence: "Follows the repository branch head", CoverageNotes: "Text files matching " + strings.Join(item.Include, ", ") + " in " + item.Repo + ".", SourceFeatures: []string{"front matter metadata", "citation URLs", "include expansion", "followable relative links"}}
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
	Commit string
}

func (p *GitDocs) Latest(ctx context.Context, collection, variant string) (provider.Release, error) {
	item := p.lookup(collection)
	if item == nil || variant != "" && variant != variantID {
		return provider.Release{}, fmt.Errorf("unknown gitdocs dataset or variant %q/%q", collection, variant)
	}
	commit, err := p.resolveCommit(ctx, item.Repo, item.Branch)
	if err != nil {
		return provider.Release{}, err
	}
	return provider.Release{Fingerprint: commit, Date: time.Now().UTC().Format("20060102"), Value: &release{Commit: commit}}, nil
}

// resolveCommit asks the GitHub REST API for the branch head and falls back to
// the anonymous smart-HTTP ref advertisement when the API is rate limited.
func (p *GitDocs) resolveCommit(ctx context.Context, repo, branch string) (string, error) {
	ref := "HEAD"
	if branch != "" {
		ref = branch
	}
	commit, apiErr := p.apiCommit(ctx, repo, ref)
	if apiErr == nil {
		return commit, nil
	}
	commit, gitErr := p.advertisedCommit(ctx, repo, branch)
	if gitErr == nil {
		return commit, nil
	}
	return "", fmt.Errorf("resolve %s head: %w", repo, errors.Join(apiErr, gitErr))
}

func (p *GitDocs) apiCommit(ctx context.Context, repo, ref string) (string, error) {
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, p.apiBase+"/repos/"+repo+"/commits/"+ref, nil)
	if err != nil {
		return "", err
	}
	request.Header.Set("Accept", "application/vnd.github.sha")
	request.Header.Set("X-GitHub-Api-Version", "2022-11-28")
	request.Header.Set("User-Agent", userAgent)
	if p.token != "" {
		request.Header.Set("Authorization", "Bearer "+p.token)
	}
	response, err := p.http.Do(request)
	if err != nil {
		return "", err
	}
	defer func() { _ = response.Body.Close() }()
	body, err := io.ReadAll(io.LimitReader(response.Body, 4096))
	if err != nil {
		return "", err
	}
	if response.StatusCode != http.StatusOK {
		return "", fmt.Errorf("GitHub API: %s", response.Status)
	}
	commit := strings.TrimSpace(string(body))
	if !shaPattern.MatchString(commit) {
		return "", fmt.Errorf("GitHub API returned an invalid commit %q", commit)
	}
	return commit, nil
}

func (p *GitDocs) advertisedCommit(ctx context.Context, repo, branch string) (string, error) {
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, p.gitBase+"/"+repo+".git/info/refs?service=git-upload-pack", nil)
	if err != nil {
		return "", err
	}
	request.Header.Set("User-Agent", "git/2.45.0 "+userAgent)
	response, err := p.http.Do(request)
	if err != nil {
		return "", err
	}
	defer func() { _ = response.Body.Close() }()
	if response.StatusCode != http.StatusOK {
		return "", fmt.Errorf("git ref advertisement: %s", response.Status)
	}
	data, err := io.ReadAll(io.LimitReader(response.Body, 256<<20))
	if err != nil {
		return "", err
	}
	want := "HEAD"
	if branch != "" {
		want = "refs/heads/" + branch
	}
	return parseAdvertisedRef(data, want)
}

// parseAdvertisedRef finds a ref in a git smart-HTTP pkt-line advertisement.
func parseAdvertisedRef(data []byte, want string) (string, error) {
	for len(data) >= 4 {
		var length int
		if _, err := fmt.Sscanf(string(data[:4]), "%04x", &length); err != nil {
			return "", fmt.Errorf("invalid pkt-line header %q", data[:4])
		}
		if length == 0 {
			data = data[4:]
			continue
		}
		if length < 4 || length > len(data) {
			return "", errors.New("truncated pkt-line")
		}
		line := strings.TrimRight(string(data[4:length]), "\n")
		data = data[length:]
		if nul := strings.IndexByte(line, 0); nul >= 0 {
			line = line[:nul]
		}
		commit, name, ok := strings.Cut(line, " ")
		if ok && name == want && shaPattern.MatchString(commit) {
			return commit, nil
		}
	}
	return "", fmt.Errorf("ref %s not advertised", want)
}

// documentIndex is the provider metadata persisted in each generation.
type documentIndex struct {
	Dataset  string     `json:"dataset"`
	Repo     string     `json:"repo"`
	Commit   string     `json:"commit"`
	Entries  []docEntry `json:"entries"`
	Complete bool       `json:"complete"`
}

type docEntry struct {
	ID       string            `json:"id"`
	Path     string            `json:"path"`
	Title    string            `json:"title"`
	URL      string            `json:"url"`
	Fragment bool              `json:"fragment,omitempty"`
	Metadata map[string]string `json:"metadata,omitempty"`
}

func (p *GitDocs) Acquire(ctx context.Context, collection, _ string, value provider.Release, stage, _ string, progress provider.Progress) (model.Manifest, error) {
	item := p.lookup(collection)
	if item == nil {
		return model.Manifest{}, fmt.Errorf("unknown gitdocs dataset %q", collection)
	}
	commit := value.Fingerprint
	if resolved, ok := value.Value.(*release); ok {
		commit = resolved.Commit
	}
	if !shaPattern.MatchString(commit) {
		return model.Manifest{}, errors.New("invalid gitdocs release metadata")
	}
	// A completed generation for the same commit is reused as-is.
	if index, err := readIndex(stage); err == nil && index.Complete && index.Commit == commit && index.Dataset == item.ID {
		return p.manifest(item, value, stage, index)
	}
	rawDir := filepath.Join(stage, rawDirectory)
	var files []string
	var err error
	for attempt := 1; attempt <= maxAcquireAttempts; attempt++ {
		if err = os.RemoveAll(rawDir); err != nil {
			return model.Manifest{}, err
		}
		files, err = p.streamArchive(ctx, item, commit, rawDir, progress)
		if err == nil || ctx.Err() != nil {
			break
		}
		progress("downloading_archive", 0, 0, "bytes", 0, fmt.Sprintf("retrying archive stream after error (attempt %d/%d): %v", attempt, maxAcquireAttempts, err))
	}
	if err != nil {
		return model.Manifest{}, err
	}
	index := documentIndex{Dataset: item.ID, Repo: item.Repo, Commit: commit, Complete: true}
	for position, relative := range files {
		if err := ctx.Err(); err != nil {
			return model.Manifest{}, err
		}
		source, readErr := os.ReadFile(filepath.Join(rawDir, filepath.FromSlash(relative)))
		if readErr != nil {
			return model.Manifest{}, readErr
		}
		entry, keep := describe(item, commit, relative, string(source))
		if !keep {
			if removeErr := os.Remove(filepath.Join(rawDir, filepath.FromSlash(relative))); removeErr != nil {
				return model.Manifest{}, removeErr
			}
			continue
		}
		index.Entries = append(index.Entries, entry)
		if (position+1)%500 == 0 || position+1 == len(files) {
			progress("parsing_documents", int64(position+1), int64(len(files)), "documents", 0, "reading titles and front matter")
		}
	}
	sort.Slice(index.Entries, func(i, j int) bool { return index.Entries[i].Path < index.Entries[j].Path })
	data, err := json.Marshal(index)
	if err != nil {
		return model.Manifest{}, err
	}
	if err := writeAtomic(filepath.Join(stage, documentsFile), data); err != nil {
		return model.Manifest{}, err
	}
	return p.manifest(item, value, stage, index)
}

func (p *GitDocs) manifest(item *Dataset, value provider.Release, stage string, index documentIndex) (model.Manifest, error) {
	var rawBytes int64
	documents := uint64(0)
	for _, entry := range index.Entries {
		if info, err := os.Stat(filepath.Join(stage, rawDirectory, filepath.FromSlash(entry.Path))); err == nil {
			rawBytes += info.Size()
		}
		if !entry.Fragment {
			documents++
		}
	}
	var metadataBytes int64
	if info, err := os.Stat(filepath.Join(stage, documentsFile)); err == nil {
		metadataBytes = info.Size()
	}
	now := time.Now().UTC()
	return model.Manifest{
		Provider: ProviderID, Variant: variantID, Dataset: item.ID, ReleaseDate: value.Date, Fingerprint: index.Commit,
		PartCount: 1, RawSize: rawBytes, ProviderMetadataSize: metadataBytes, DocumentCount: documents, PublishedAt: now,
		Site: model.DatasetMetadata{Name: item.Name, Description: item.Description, Project: item.Project, ContentType: "Technical documentation", Profile: profile(*item), Language: language(item.Language), OnlineSourceURL: "https://github.com/" + item.Repo + "/tree/" + index.Commit, SourceDocuments: documents, MetadataUpdatedAt: now},
	}, nil
}

type countingReader struct {
	reader io.Reader
	count  atomic.Int64
}

func (c *countingReader) Read(buffer []byte) (int, error) {
	n, err := c.reader.Read(buffer)
	c.count.Add(int64(n))
	return n, err
}

func (p *GitDocs) streamArchive(ctx context.Context, item *Dataset, commit, rawDir string, progress provider.Progress) ([]string, error) {
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, p.codeloadBase+"/"+item.Repo+"/tar.gz/"+commit, nil)
	if err != nil {
		return nil, err
	}
	request.Header.Set("User-Agent", userAgent)
	response, err := p.http.Do(request)
	if err != nil {
		return nil, err
	}
	defer func() { _ = response.Body.Close() }()
	if response.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("download %s@%s archive: %s", item.Repo, commit[:7], response.Status)
	}
	counter := &countingReader{reader: response.Body}
	started := time.Now()
	lastReport := time.Time{}
	report := func(kept int) {
		if time.Since(lastReport) < time.Second {
			return
		}
		lastReport = time.Now()
		read := counter.count.Load()
		rate := float64(read) / max(time.Since(started).Seconds(), 0.001)
		progress("downloading_archive", read, max(response.ContentLength, 0), "bytes", rate, fmt.Sprintf("streaming %s@%s, %d text files kept", item.Repo, commit[:7], kept))
	}
	return extractArchive(counter, rawDir, func(name string) bool { return selectFile(item, name) }, report)
}

// extractArchive streams a GitHub tar.gz archive and writes the selected
// regular files below rawDir, stripping the archive's top-level directory.
func extractArchive(source io.Reader, rawDir string, keep func(string) bool, report func(int)) ([]string, error) {
	decompressed, err := gzip.NewReader(source)
	if err != nil {
		return nil, fmt.Errorf("open archive: %w", err)
	}
	defer func() { _ = decompressed.Close() }()
	reader := tar.NewReader(decompressed)
	var files []string
	for {
		header, err := reader.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return nil, fmt.Errorf("read archive: %w", err)
		}
		if report != nil {
			report(len(files))
		}
		if header.Typeflag != tar.TypeReg || header.Size > maxTextFile {
			continue
		}
		relative, ok := archivePath(header.Name)
		if !ok || !keep(relative) {
			continue
		}
		destination := filepath.Join(rawDir, filepath.FromSlash(relative))
		if err := os.MkdirAll(filepath.Dir(destination), 0o755); err != nil {
			return nil, err
		}
		file, err := os.OpenFile(destination, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0o644)
		if err != nil {
			return nil, err
		}
		_, copyErr := io.Copy(file, reader)
		if err := errors.Join(copyErr, file.Close()); err != nil {
			return nil, err
		}
		files = append(files, relative)
	}
	return files, nil
}

// archivePath removes the "<repo>-<sha>/" root and rejects unsafe names.
func archivePath(name string) (string, bool) {
	_, relative, found := strings.Cut(strings.TrimPrefix(name, "./"), "/")
	if !found || relative == "" || strings.HasPrefix(relative, "/") || strings.Contains(relative, "\\") {
		return "", false
	}
	cleaned := path.Clean(relative)
	if cleaned != relative || cleaned == "." || cleaned == ".." || strings.HasPrefix(cleaned, "../") {
		return "", false
	}
	return cleaned, true
}

func isMarkdown(name string) bool {
	switch strings.ToLower(path.Ext(name)) {
	case ".md", ".markdown", ".mdx":
		return true
	}
	return false
}

func isYAML(name string) bool {
	switch strings.ToLower(path.Ext(name)) {
	case ".yml", ".yaml":
		return true
	}
	return false
}

func selectFile(item *Dataset, name string) bool {
	if !isMarkdown(name) && !isYAML(name) {
		return false
	}
	if !matchAny(item.Include, name) || matchAny(item.Exclude, name) {
		return false
	}
	return true
}

func matchAny(patterns []string, name string) bool {
	for _, pattern := range patterns {
		if matchGlob(pattern, name) {
			return true
		}
	}
	return false
}

// matchGlob matches slash-separated paths; "**" spans any number of segments.
func matchGlob(pattern, name string) bool {
	return matchSegments(strings.Split(pattern, "/"), strings.Split(name, "/"))
}

func matchSegments(pattern, name []string) bool {
	for len(pattern) > 0 {
		if pattern[0] == "**" {
			if len(pattern) == 1 {
				return true
			}
			for skip := 0; skip <= len(name); skip++ {
				if matchSegments(pattern[1:], name[skip:]) {
					return true
				}
			}
			return false
		}
		if len(name) == 0 {
			return false
		}
		if ok, err := path.Match(pattern[0], name[0]); err != nil || !ok {
			return false
		}
		pattern, name = pattern[1:], name[1:]
	}
	return len(name) == 0
}

func documentID(relative string) string {
	digest := sha256.Sum256([]byte(relative))
	return "g" + strings.ToLower(base32.StdEncoding.WithPadding(base32.NoPadding).EncodeToString(digest[:10]))
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
	temporary := destination + ".tmp"
	if err := os.WriteFile(temporary, data, 0o644); err != nil {
		return err
	}
	return os.Rename(temporary, destination)
}
