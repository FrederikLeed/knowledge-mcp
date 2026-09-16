package markdowndoc

import (
	"strings"
	"testing"

	"github.com/lkarlslund/knowledge-mcp/internal/model"
)

func TestSplitFrontMatter(t *testing.T) {
	t.Parallel()
	fields, body := SplitFrontMatter("\ufeff---\r\ntitle: Configure MFA\r\nms.date: 03/14/2025\r\ndescription: \"How MFA works\"\r\nms.custom:\r\n  - a\r\n  - b\r\ndate: 2024-05-01\r\n---\r\n# Heading\r\nBody\r\n")
	if fields["title"] != "Configure MFA" || fields["ms.date"] != "03/14/2025" || fields["description"] != "How MFA works" || fields["ms.custom"] != "a, b" || fields["date"] != "2024-05-01" {
		t.Fatalf("fields = %#v", fields)
	}
	if body != "# Heading\nBody\n" {
		t.Fatalf("body = %q", body)
	}

	// Liquid templates make the block invalid YAML; the line parser still
	// recovers scalar keys and the body is not lost.
	fields, body = SplitFrontMatter("---\ntitle: \"Light\"\nha_category: {% if x %}Lights{% endif %}\n  bad: : indent\n---\nText\n")
	if fields["title"] != "Light" || body != "Text\n" {
		t.Fatalf("fallback fields = %#v body = %q", fields, body)
	}

	fields, body = SplitFrontMatter("No front matter\n---\nnot: yaml\n")
	if len(fields) != 0 || !strings.HasPrefix(body, "No front matter") {
		t.Fatalf("plain document = %#v %q", fields, body)
	}
	// An unterminated block is treated as body, not swallowed.
	fields, body = SplitFrontMatter("---\ntitle: x\nno end\n")
	if len(fields) != 0 || !strings.Contains(body, "no end") {
		t.Fatalf("unterminated = %#v %q", fields, body)
	}
}

func TestFirstHeadingIgnoresCodeAndCleansMarkup(t *testing.T) {
	t.Parallel()
	body := "```powershell\n# not a heading\n```\n\n## Second level\n\n# [Get-Process](link.md) **cmdlet** {#anchor}\n"
	if got := FirstHeading(body); got != "Get-Process cmdlet" {
		t.Fatalf("FirstHeading = %q", got)
	}
	if got := FirstHeading("    # indented code\ntext"); got != "" {
		t.Fatalf("indented code heading = %q", got)
	}
}

func TestSectionsAndPaging(t *testing.T) {
	t.Parallel()
	content := "# Title\n\nIntro\n\n## Install the module\n\nSteps\n\n### Requirements\n\nPS 7\n\n## Usage\n\nRun it\n"
	outline := Outline(content)
	if len(outline) != 4 || outline[1].Anchor != "install-the-module" || outline[2].Level != 3 {
		t.Fatalf("outline = %#v", outline)
	}
	section, found := ExtractSection(content, "install-the-module")
	if !found || !strings.Contains(section, "PS 7") || strings.Contains(section, "Run it") {
		t.Fatalf("section = %q %v", section, found)
	}
	document := Page(model.Document{ID: "x"}, content, model.ReadOptions{Section: "Missing"})
	if document.SectionFound == nil || *document.SectionFound || len(document.Sections) != 4 || document.Content != "" {
		t.Fatalf("missing section = %#v", document)
	}
	var rebuilt strings.Builder
	for offset := 0; ; {
		chunk := Page(model.Document{}, content, model.ReadOptions{Offset: offset, MaxChars: 20, AlignBoundaries: true})
		rebuilt.WriteString(chunk.Content)
		if !chunk.Truncated {
			break
		}
		if chunk.NextOffset <= offset {
			t.Fatalf("paging did not progress: %#v", chunk)
		}
		offset = chunk.NextOffset
	}
	if rebuilt.String() != content {
		t.Fatalf("paging lost content: %q", rebuilt.String())
	}
}
