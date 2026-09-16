// Package markdowndoc holds provider-neutral helpers for Markdown sources:
// front matter parsing, title detection, heading outlines, targeted section
// extraction, and paginated document assembly.
package markdowndoc

import (
	"regexp"
	"strings"
	"time"

	"github.com/lkarlslund/knowledge-mcp/internal/knowledgeindex"
	"github.com/lkarlslund/knowledge-mcp/internal/model"
	"gopkg.in/yaml.v3"
)

var (
	frontMatterKeyRE = regexp.MustCompile(`^([A-Za-z0-9_.\-]+):\s*(.*)$`)
	atxHeadingRE     = regexp.MustCompile(`^(#{1,6})[ \t]+(.+?)[ \t]*#*[ \t]*$`)
	headingAnchorRE  = regexp.MustCompile(`\s*\{#([^}]+)\}\s*$`)
	markdownLinkRE   = regexp.MustCompile(`\[([^\]]*)\]\([^)]*\)`)
	anchorStripRE    = regexp.MustCompile(`[^\p{L}\p{N}\- ]+`)
)

// SplitFrontMatter separates a leading YAML front matter block ("---" fenced)
// from the Markdown body. Scalar values are returned as strings; nested values
// are ignored. Invalid YAML falls back to a tolerant line parser so template
// syntax inside front matter never hides the document.
func SplitFrontMatter(source string) (map[string]string, string) {
	source = strings.TrimPrefix(source, "\ufeff")
	normalized := strings.ReplaceAll(source, "\r\n", "\n")
	if !strings.HasPrefix(normalized, "---\n") {
		return map[string]string{}, normalized
	}
	rest := normalized[4:]
	end := -1
	offset := 0
	for _, line := range strings.SplitAfter(rest, "\n") {
		trimmed := strings.TrimRight(line, " \t\n")
		if trimmed == "---" || trimmed == "..." {
			end = offset
			offset += len(line)
			break
		}
		offset += len(line)
	}
	if end < 0 {
		return map[string]string{}, normalized
	}
	block, body := rest[:end], rest[offset:]
	fields := map[string]string{}
	var parsed map[string]any
	if err := yaml.Unmarshal([]byte(block), &parsed); err == nil {
		for key, value := range parsed {
			switch typed := value.(type) {
			case string:
				fields[key] = strings.TrimSpace(typed)
			case time.Time:
				fields[key] = typed.UTC().Format("2006-01-02")
			case int, int64, float64, bool:
				fields[key] = strings.TrimSpace(yamlScalar(typed))
			case []any:
				var parts []string
				for _, item := range typed {
					if text, ok := item.(string); ok && strings.TrimSpace(text) != "" {
						parts = append(parts, strings.TrimSpace(text))
					}
				}
				if len(parts) > 0 {
					fields[key] = strings.Join(parts, ", ")
				}
			}
		}
		return fields, body
	}
	for _, line := range strings.Split(block, "\n") {
		if match := frontMatterKeyRE.FindStringSubmatch(line); match != nil {
			value := strings.TrimSpace(match[2])
			value = strings.Trim(value, `"'`)
			if value != "" && value != "|" && value != ">" {
				fields[match[1]] = value
			}
		}
	}
	return fields, body
}

func yamlScalar(value any) string {
	data, err := yaml.Marshal(value)
	if err != nil {
		return ""
	}
	return string(data)
}

// FirstHeading returns the text of the first level-one ATX heading outside
// fenced code blocks.
func FirstHeading(body string) string {
	for _, heading := range headings(body) {
		if heading.level == 1 {
			return heading.text
		}
	}
	return ""
}

// CleanHeading removes Markdown link syntax, emphasis, and explicit anchors.
func CleanHeading(value string) string {
	value = headingAnchorRE.ReplaceAllString(value, "")
	value = markdownLinkRE.ReplaceAllString(value, "$1")
	value = strings.NewReplacer("**", "", "__", "", "`", "").Replace(value)
	return strings.Join(strings.Fields(value), " ")
}

type heading struct {
	level int
	text  string
	start int
}

func headings(body string) []heading {
	var result []heading
	fence := ""
	offset := 0
	for _, line := range strings.SplitAfter(body, "\n") {
		trimmed := strings.TrimRight(line, "\r\n")
		stripped := strings.TrimLeft(trimmed, " ")
		if fence != "" {
			if strings.HasPrefix(stripped, fence) {
				fence = ""
			}
		} else if strings.HasPrefix(stripped, "```") || strings.HasPrefix(stripped, "~~~") {
			fence = stripped[:3]
		} else if len(trimmed)-len(stripped) < 4 {
			if match := atxHeadingRE.FindStringSubmatch(stripped); match != nil {
				if text := CleanHeading(match[2]); text != "" {
					result = append(result, heading{level: len(match[1]), text: text, start: offset})
				}
			}
		}
		offset += len(line)
	}
	return result
}

// Outline lists the headings of a Markdown document.
func Outline(body string) []model.DocumentSection {
	items := headings(body)
	sections := make([]model.DocumentSection, len(items))
	for index, item := range items {
		sections[index] = model.DocumentSection{Heading: item.text, Anchor: Anchor(item.text), Level: item.level}
	}
	return sections
}

// Anchor returns a GitHub-style heading anchor.
func Anchor(text string) string {
	text = strings.ToLower(strings.TrimSpace(text))
	text = anchorStripRE.ReplaceAllString(text, "")
	return strings.ReplaceAll(text, " ", "-")
}

// ExtractSection returns the requested section and its subsections, matched by
// heading text or anchor.
func ExtractSection(body, requested string) (string, bool) {
	target := normalizeSection(requested)
	items := headings(body)
	for index, item := range items {
		if target != normalizeSection(item.text) && target != normalizeSection(Anchor(item.text)) {
			continue
		}
		end := len(body)
		for next := index + 1; next < len(items); next++ {
			if items[next].level <= item.level {
				end = items[next].start
				break
			}
		}
		return strings.TrimSpace(body[item.start:end]) + "\n", true
	}
	return "", false
}

func normalizeSection(value string) string {
	value = strings.TrimSpace(strings.TrimPrefix(value, "#"))
	value = strings.NewReplacer("_", " ", "-", " ").Replace(value)
	return strings.ToLower(strings.Join(strings.Fields(value), " "))
}

// Page assembles a model.Document from full Markdown content, applying the
// section, outline, offset, and length options of a read request.
func Page(document model.Document, content string, options model.ReadOptions) model.Document {
	section := strings.TrimSpace(options.Section)
	if section != "" {
		extracted, found := ExtractSection(content, section)
		document.Section, document.SectionFound = section, &found
		if !found || options.IncludeOutline {
			document.Sections = Outline(content)
		}
		content = extracted
	} else if options.IncludeOutline {
		document.Sections = Outline(content)
	}
	if len(document.Sections) > 200 {
		document.Sections = document.Sections[:200]
		document.OutlineTruncated = true
	}
	maximum := options.MaxChars
	if maximum <= 0 {
		maximum = knowledgeindex.DefaultReadCharacters
	}
	runes := []rune(content)
	start := min(max(options.Offset, 0), len(runes))
	end := min(start+maximum, len(runes))
	if options.AlignBoundaries && end < len(runes) {
		for index := end; index > start+(end-start)/2; index-- {
			if runes[index-1] == '\n' {
				end = index
				break
			}
		}
	}
	document.Content = string(runes[start:end])
	document.Offset, document.ReturnedChars, document.TotalChars = start, end-start, len(runes)
	document.Truncated = end < len(runes)
	if document.Truncated {
		document.NextOffset = end
	}
	return document
}
