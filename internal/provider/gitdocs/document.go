package gitdocs

import (
	"context"
	"errors"
	"fmt"
	"net/url"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/lkarlslund/knowledge-mcp/internal/markdowndoc"
	"github.com/lkarlslund/knowledge-mcp/internal/model"
	"github.com/lkarlslund/knowledge-mcp/internal/provider"
	"gopkg.in/yaml.v3"
)

const maxIncludeDepth = 3

var (
	includeRE    = regexp.MustCompile(`\[!(?i:include)\s*\[[^\]]*\]\(\s*<?([^)>\s]+)>?\s*\)\s*\]`)
	inlineLinkRE = regexp.MustCompile(`(\]\()(<[^>]*>|[^)\s]+)((?:\s+"[^"]*")?\))`)
	schemeRE     = regexp.MustCompile(`^[A-Za-z][A-Za-z0-9+.-]*:`)
	yamlMimeRE   = regexp.MustCompile(`^#{3}\s*YamlMime:\s*([A-Za-z0-9_.-]+)`)
)

// Metadata keys copied from front matter into search records.
var keptMetadata = []string{"description", "ms.date", "ms.topic", "ms.service", "ms.subservice", "date", "ha_category", "ha_domain", "ha_iot_class", "ha_release", "keywords", "tags"}

// YamlMime document types that are navigation shells rather than content.
var skippedYamlMime = map[string]bool{"landing": true, "hub": true, "contextobject": true, "achievements": true, "toc": true, "yamldocument": true}

// describe builds the persisted entry for one extracted file. It returns
// false for files that must not be kept (e.g. non-content YAML).
func describe(item *Dataset, commit, relative, source string) (docEntry, bool) {
	entry := docEntry{ID: documentID(relative), Path: relative, URL: citationURL(item, commit, relative), Fragment: matchAny(item.Fragments, relative)}
	var fields map[string]string
	var body string
	if isYAML(relative) {
		mime, ok := yamlMime(source)
		if !ok || skippedYamlMime[strings.ToLower(mime)] {
			return docEntry{}, false
		}
		fields = yamlFields(source)
		fields["yamlmime"] = mime
	} else {
		fields, body = markdowndoc.SplitFrontMatter(source)
	}
	entry.Title = firstNonEmpty(fields["title"], fields["metadata.title"], markdowndoc.FirstHeading(body), fields["sidebar_label"], titleFromPath(relative))
	entry.Title = markdowndoc.CleanHeading(entry.Title)
	for _, key := range keptMetadata {
		if value := strings.TrimSpace(fields[key]); value != "" {
			if entry.Metadata == nil {
				entry.Metadata = map[string]string{}
			}
			entry.Metadata[key] = value
		}
	}
	if mime := fields["yamlmime"]; mime != "" {
		if entry.Metadata == nil {
			entry.Metadata = map[string]string{}
		}
		entry.Metadata["yamlmime"] = mime
	}
	return entry, true
}

func firstNonEmpty(values ...string) string {
	for _, value := range values {
		if value = strings.TrimSpace(value); value != "" {
			return value
		}
	}
	return ""
}

func titleFromPath(relative string) string {
	base := strings.TrimSuffix(path.Base(relative), path.Ext(relative))
	if strings.EqualFold(base, "index") || strings.EqualFold(base, "readme") {
		if directory := path.Base(path.Dir(relative)); directory != "." && directory != "/" {
			base = directory
		}
	}
	return strings.NewReplacer("-", " ", "_", " ").Replace(base)
}

func yamlMime(source string) (string, bool) {
	firstLine, _, _ := strings.Cut(strings.TrimPrefix(source, "\ufeff"), "\n")
	match := yamlMimeRE.FindStringSubmatch(strings.TrimSpace(firstLine))
	if match == nil {
		return "", false
	}
	return match[1], true
}

// yamlFields returns top-level scalar fields plus metadata.* scalars of a
// DocFX YAML document.
func yamlFields(source string) map[string]string {
	fields := map[string]string{}
	var root map[string]any
	if err := yaml.Unmarshal([]byte(source), &root); err != nil {
		return fields
	}
	for key, value := range root {
		if text, ok := value.(string); ok {
			fields[key] = strings.TrimSpace(text)
		}
	}
	if metadata, ok := root["metadata"].(map[string]any); ok {
		for key, value := range metadata {
			switch typed := value.(type) {
			case string:
				fields["metadata."+key] = strings.TrimSpace(typed)
				if _, exists := fields[key]; !exists {
					fields[key] = strings.TrimSpace(typed)
				}
			case time.Time:
				fields[key] = typed.UTC().Format("2006-01-02")
			}
		}
	}
	return fields
}

// yamlMarkdown flattens DocFX YAML content (FAQ, conceptual YAML) into
// readable Markdown: titles and questions become headings, text becomes
// paragraphs, and identifiers, links, and layout values are dropped.
func yamlMarkdown(source string) string {
	var root yaml.Node
	if err := yaml.Unmarshal([]byte(source), &root); err != nil {
		return source
	}
	var output strings.Builder
	var walk func(node *yaml.Node, key string, depth int)
	walk = func(node *yaml.Node, key string, depth int) {
		switch node.Kind {
		case yaml.DocumentNode, yaml.SequenceNode:
			for _, child := range node.Content {
				walk(child, key, depth)
			}
		case yaml.MappingNode:
			for index := 0; index+1 < len(node.Content); index += 2 {
				childKey := node.Content[index].Value
				switch childKey {
				case "metadata", "uid", "url", "href", "image", "src", "alt", "layout", "brand", "type", "ms.date", "ms.topic", "ms.service", "author", "ms.author", "manager", "ms.collection", "ms.custom", "ms.subservice", "displayType", "itemType", "icon":
					continue
				}
				walk(node.Content[index+1], childKey, depth+1)
			}
		case yaml.ScalarNode:
			value := strings.TrimSpace(node.Value)
			if value == "" {
				return
			}
			switch key {
			case "title":
				fmt.Fprintf(&output, "\n%s %s\n\n", strings.Repeat("#", min(max(depth, 1), 3)), value)
			case "name", "question":
				fmt.Fprintf(&output, "\n### %s\n\n", value)
			default:
				output.WriteString(value)
				output.WriteString("\n\n")
			}
		case yaml.AliasNode:
		}
	}
	walk(&root, "", 0)
	return strings.TrimSpace(output.String()) + "\n"
}

// citationURL maps a repository path to its published page, or to the GitHub
// blob URL at the indexed commit when no rule applies.
func citationURL(item *Dataset, commit, relative string) string {
	for _, rule := range item.URLRules {
		rest, ok := matchPrefix(rule.Prefix, relative)
		if !ok {
			continue
		}
		extension := path.Ext(rest)
		rest = strings.TrimSuffix(rest, extension)
		if rule.Lowercase {
			rest = strings.ToLower(rest)
		}
		trailing := rule.TrailingSlash
		if base := path.Base(rest); strings.EqualFold(base, "index") {
			rest = strings.TrimSuffix(strings.TrimSuffix(rest, base), "/")
			trailing = true
		}
		target := strings.TrimRight(rule.Base, "/") + "/" + escapePath(rest)
		if trailing && !strings.HasSuffix(target, "/") {
			target += "/"
		}
		return target + rule.Query
	}
	return blobURL(item.Repo, commit, relative)
}

func blobURL(repo, commit, relative string) string {
	return "https://github.com/" + repo + "/blob/" + commit + "/" + escapePath(relative)
}

func escapePath(value string) string {
	if value == "" {
		return ""
	}
	parts := strings.Split(value, "/")
	for index, part := range parts {
		parts[index] = url.PathEscape(part)
	}
	return strings.Join(parts, "/")
}

// matchPrefix matches a slash-terminated prefix whose segments may be "*".
func matchPrefix(prefix, relative string) (string, bool) {
	segments := strings.Split(strings.Trim(prefix, "/"), "/")
	if strings.Trim(prefix, "/") == "" {
		return relative, true
	}
	parts := strings.Split(relative, "/")
	if len(parts) <= len(segments) {
		return "", false
	}
	for index, segment := range segments {
		if ok, err := path.Match(segment, parts[index]); err != nil || !ok {
			return "", false
		}
	}
	return strings.Join(parts[len(segments):], "/"), true
}

// corpus serves one installed generation.
type corpus struct {
	path    string
	item    *Dataset
	index   documentIndex
	records []int
	byID    map[string]int
	byPath  map[string]int
}

func (p *GitDocs) OpenCorpus(directory string, manifest model.Manifest) (provider.Corpus, error) {
	item := p.lookup(manifest.Dataset)
	if item == nil {
		return nil, fmt.Errorf("gitdocs dataset %q is no longer in the catalog", manifest.Dataset)
	}
	index, err := readIndex(directory)
	if err != nil {
		return nil, err
	}
	return newCorpus(directory, item, index), nil
}

func newCorpus(directory string, item *Dataset, index documentIndex) *corpus {
	c := &corpus{path: directory, item: item, index: index, byID: make(map[string]int, len(index.Entries)), byPath: make(map[string]int, len(index.Entries))}
	for position, entry := range index.Entries {
		c.byID[entry.ID] = position
		c.byPath[entry.Path] = position
		if !entry.Fragment {
			c.records = append(c.records, position)
		}
	}
	return c
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
		if err != nil || parsed < 0 || parsed > len(c.records) {
			return fmt.Errorf("invalid gitdocs scan cursor %q", after)
		}
		start = parsed
	}
	for index := start; index < len(c.records); index++ {
		if err := ctx.Err(); err != nil {
			return err
		}
		entry := c.index.Entries[c.records[index]]
		record := c.record(entry)
		if bodies {
			body, err := c.body(entry)
			if err != nil {
				return err
			}
			record.Body = body
		}
		if err := sink(record, provider.ScanPosition{Cursor: strconv.Itoa(index + 1), Completed: int64(index + 1), Total: int64(len(c.records)), Units: "documents", Boundary: true}); err != nil {
			return err
		}
	}
	return nil
}

func (c *corpus) record(entry docEntry) provider.Record {
	keywords := append([]string{c.item.Project, c.item.Repo}, c.item.Topics...)
	for _, key := range []string{"ms.service", "ms.subservice", "ms.topic", "ha_category", "ha_domain", "keywords", "tags"} {
		if value := entry.Metadata[key]; value != "" {
			keywords = append(keywords, value)
		}
	}
	metadata := map[string]string{"path": entry.Path, "repo": c.index.Repo, "commit": c.index.Commit}
	for key, value := range entry.Metadata {
		metadata[key] = value
	}
	record := provider.Record{ID: entry.ID, Title: entry.Title, URL: entry.URL, Locator: entry.Path, Primary: true, Identifiers: []string{entry.Path}, Keywords: keywords, RankWeight: 1, Metadata: metadata}
	if date := parseDate(firstNonEmpty(entry.Metadata["ms.date"], entry.Metadata["date"])); date != nil {
		record.Temporal.ModifiedAt = date
	}
	return record
}

func parseDate(value string) *time.Time {
	value = strings.TrimSpace(value)
	for _, layout := range []string{"01/02/2006", "1/2/2006", "2006-01-02", time.RFC3339, "2006-01-02 15:04:05 -0700", "2006-01-02 15:04:05"} {
		if parsed, err := time.Parse(layout, value); err == nil {
			parsed = parsed.UTC()
			return &parsed
		}
	}
	return nil
}

func (c *corpus) entryFor(record provider.Record) (docEntry, bool) {
	if position, ok := c.byID[record.ID]; ok {
		return c.index.Entries[position], true
	}
	if position, ok := c.byPath[record.Locator]; ok {
		return c.index.Entries[position], true
	}
	return docEntry{}, false
}

func (c *corpus) raw(entry docEntry) (string, error) {
	data, err := os.ReadFile(filepath.Join(c.path, rawDirectory, filepath.FromSlash(entry.Path)))
	if errors.Is(err, os.ErrNotExist) {
		return "", provider.ErrDocumentNotFound
	}
	return string(data), err
}

// body renders the searchable Markdown body: front matter removed, includes
// expanded, and links made followable.
func (c *corpus) body(entry docEntry) (string, error) {
	return c.renderBody(entry, 0)
}

func (c *corpus) renderBody(entry docEntry, depth int) (string, error) {
	source, err := c.raw(entry)
	if err != nil {
		return "", err
	}
	var body string
	if isYAML(entry.Path) {
		body = yamlMarkdown(source)
	} else {
		_, body = markdowndoc.SplitFrontMatter(source)
	}
	body = includeRE.ReplaceAllStringFunc(body, func(match string) string {
		target := includeRE.FindStringSubmatch(match)[1]
		resolved, ok := c.resolve(entry.Path, target)
		if !ok || depth >= maxIncludeDepth {
			return match
		}
		included, includeErr := c.renderBody(c.index.Entries[resolved], depth+1)
		if includeErr != nil {
			return match
		}
		return strings.TrimSpace(included)
	})
	if depth == 0 {
		body = c.rewriteLinks(entry, body)
	}
	return body, nil
}

// resolve maps a relative link target to a stored entry.
func (c *corpus) resolve(from, target string) (int, bool) {
	target = strings.Trim(strings.TrimSpace(target), "<>")
	if target == "" || schemeRE.MatchString(target) || strings.HasPrefix(target, "/") || strings.HasPrefix(target, "#") {
		return 0, false
	}
	target, _, _ = strings.Cut(target, "#")
	target, _, _ = strings.Cut(target, "?")
	if unescaped, err := url.PathUnescape(target); err == nil {
		target = unescaped
	}
	joined := path.Clean(path.Join(path.Dir(from), target))
	for _, candidate := range []string{joined, joined + ".md", joined + ".markdown", joined + ".mdx", joined + "/index.md", joined + "/README.md", strings.TrimSuffix(joined, "/") + ".md"} {
		if position, ok := c.byPath[candidate]; ok {
			return position, true
		}
	}
	return 0, false
}

func (c *corpus) rewriteLinks(entry docEntry, body string) string {
	return inlineLinkRE.ReplaceAllStringFunc(body, func(match string) string {
		parts := inlineLinkRE.FindStringSubmatch(match)
		target := strings.Trim(parts[2], "<>")
		switch {
		case target == "", strings.HasPrefix(target, "#"), schemeRE.MatchString(target), strings.HasPrefix(target, "//"):
			return match
		}
		from := entry.Path
		if strings.HasPrefix(target, "/") {
			// Site-absolute links either name a repository file (Docusaurus
			// "/docs/x.md") or a published page on the documentation site.
			from = ""
			if _, ok := c.resolve(from, strings.TrimPrefix(target, "/")); !ok {
				if c.item.SiteRoot == "" {
					return match
				}
				return parts[1] + c.item.SiteRoot + target + parts[3]
			}
			target = strings.TrimPrefix(target, "/")
		}
		if position, ok := c.resolve(from, target); ok {
			linked := c.index.Entries[position]
			if !linked.Fragment {
				link := "knowledge-read://read?dataset=" + url.QueryEscape(c.item.ID) + "&id=" + linked.ID
				if _, fragment, found := strings.Cut(target, "#"); found && fragment != "" {
					link += "&section=" + url.QueryEscape(fragment)
				}
				return parts[1] + link + ` "Call knowledge_read with dataset=` + c.item.ID + ` and id=` + linked.ID + `"` + parts[3]
			}
		}
		clean, _, _ := strings.Cut(target, "#")
		clean, _, _ = strings.Cut(clean, "?")
		if clean == "" {
			return match
		}
		return parts[1] + blobURL(c.index.Repo, c.index.Commit, path.Clean(path.Join(path.Dir(from), clean))) + parts[3]
	})
}

func (c *corpus) Read(_ context.Context, record provider.Record, options model.ReadOptions) (model.Document, error) {
	entry, ok := c.entryFor(record)
	if !ok || entry.Fragment {
		return model.Document{}, provider.ErrDocumentNotFound
	}
	var content string
	format := options.Format
	switch format {
	case "source":
		raw, err := c.raw(entry)
		if err != nil {
			return model.Document{}, err
		}
		content = raw
	case "text":
		body, err := c.body(entry)
		if err != nil {
			return model.Document{}, err
		}
		content = body
	default:
		format = "markdown"
		body, err := c.body(entry)
		if err != nil {
			return model.Document{}, err
		}
		content = c.header(entry, body) + body
	}
	document := model.Document{TemporalMetadata: record.Temporal, ID: entry.ID, Title: entry.Title, URL: entry.URL, Format: format}
	return markdowndoc.Page(document, content, options), nil
}

func (c *corpus) header(entry docEntry, body string) string {
	var output strings.Builder
	if markdowndoc.FirstHeading(body) == "" {
		fmt.Fprintf(&output, "# %s\n\n", entry.Title)
	}
	fmt.Fprintf(&output, "**Source:** %s  \n", entry.URL)
	fmt.Fprintf(&output, "**Repository:** %s (`%s`, commit %s)  \n", c.index.Repo, entry.Path, shortCommit(c.index.Commit))
	if value := firstNonEmpty(entry.Metadata["ms.date"], entry.Metadata["date"]); value != "" {
		fmt.Fprintf(&output, "**Updated:** %s  \n", value)
	}
	if value := entry.Metadata["description"]; value != "" {
		fmt.Fprintf(&output, "**Summary:** %s  \n", value)
	}
	output.WriteString("\n")
	return output.String()
}

func shortCommit(commit string) string {
	if len(commit) > 12 {
		return commit[:12]
	}
	return commit
}
