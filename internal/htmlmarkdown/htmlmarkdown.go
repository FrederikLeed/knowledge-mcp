// Package htmlmarkdown converts HTML documents to compact Markdown. It is shared
// by providers that store HTML sources (Kiwix ZIM archives, curated web pages).
// Link targets are rewritten by a provider-supplied callback so each provider
// keeps control over its own followable link scheme.
package htmlmarkdown

import (
	"bytes"
	"regexp"
	"strings"

	"golang.org/x/net/html"
)

// LinkFunc maps a raw href attribute to the Markdown link target.
type LinkFunc func(href string) string

var markdownSpace = regexp.MustCompile(`[ \t]+`)

// blockText elements separate words in flattened text (e.g. table cells).
var blockText = map[string]bool{"br": true, "p": true, "div": true, "li": true, "td": true, "th": true, "tr": true, "h1": true, "h2": true, "h3": true, "h4": true, "h5": true, "h6": true, "ul": true, "ol": true}

// Options tune conversion.
type Options struct {
	// Link rewrites href values. Nil leaves them unchanged.
	Link LinkFunc
	// Skip reports element nodes whose whole subtree must be omitted, in
	// addition to scripts, styles, and other non-content elements.
	Skip func(*html.Node) bool
}

// Convert renders an HTML source to Markdown.
func Convert(source []byte, options Options) string {
	document, err := html.Parse(bytes.NewReader(source))
	if err != nil {
		return strings.TrimSpace(string(source))
	}
	return ConvertNode(document, options)
}

// ConvertNode renders an already parsed HTML node to Markdown.
func ConvertNode(node *html.Node, options Options) string {
	var output bytes.Buffer
	renderNode(&output, node, options, 0)
	result := strings.ReplaceAll(output.String(), "\u00a0", " ")
	// Spacer paragraphs (e.g. <p>&nbsp;</p>) become whitespace-only lines.
	lines := strings.Split(result, "\n")
	for index, line := range lines {
		if strings.TrimSpace(line) == "" {
			lines[index] = ""
		}
	}
	result = strings.Join(lines, "\n")
	for strings.Contains(result, "\n\n\n") {
		result = strings.ReplaceAll(result, "\n\n\n", "\n\n")
	}
	return strings.TrimSpace(result)
}

func renderNode(output *bytes.Buffer, node *html.Node, options Options, listDepth int) {
	if node.Type == html.TextNode {
		text := markdownSpace.ReplaceAllString(node.Data, " ")
		output.WriteString(text)
		return
	}
	if node.Type == html.ElementNode && (node.Data == "script" || node.Data == "style" || node.Data == "noscript" || node.Data == "svg" || options.Skip != nil && options.Skip(node)) {
		return
	}
	if node.Type != html.ElementNode {
		for child := node.FirstChild; child != nil; child = child.NextSibling {
			renderNode(output, child, options, listDepth)
		}
		return
	}
	name := strings.ToLower(node.Data)
	switch name {
	case "h1", "h2", "h3", "h4", "h5", "h6":
		level := int(name[1] - '0')
		ensureBlankLine(output)
		output.WriteString(strings.Repeat("#", level) + " ")
	case "p", "div", "section", "article", "header", "footer", "blockquote":
		ensureBlankLine(output)
	case "table":
		renderTable(output, node)
		ensureBlankLine(output)
		return
	case "br":
		output.WriteString("  \n")
	case "strong", "b":
		output.WriteString("**")
	case "em", "i":
		output.WriteString("*")
	case "code":
		output.WriteByte('`')
	case "pre":
		output.WriteString("```\n")
	case "ul", "ol":
		ensureNewline(output)
		listDepth++
	case "li":
		ensureNewline(output)
		output.WriteString(strings.Repeat("  ", max(0, listDepth-1)) + "- ")
	case "a":
		label := strings.TrimSpace(NodeText(node))
		href := Attribute(node, "href")
		if label != "" {
			if options.Link != nil {
				href = options.Link(href)
			}
			output.WriteString("[")
			output.WriteString(EscapeMarkdown(label))
			output.WriteString("](")
			output.WriteString(href)
			output.WriteString(")")
		}
		return
	case "img":
		if alt := strings.TrimSpace(Attribute(node, "alt")); alt != "" {
			output.WriteString("[Image: " + EscapeMarkdown(alt) + "]")
		}
		return
	}
	for child := node.FirstChild; child != nil; child = child.NextSibling {
		renderNode(output, child, options, listDepth)
	}
	switch name {
	case "strong", "b":
		output.WriteString("**")
	case "em", "i":
		output.WriteString("*")
	case "code":
		output.WriteByte('`')
	case "pre":
		output.WriteString("\n```\n")
	case "p", "div", "section", "article", "header", "footer", "blockquote", "h1", "h2", "h3", "h4", "h5", "h6", "ul", "ol":
		ensureBlankLine(output)
	case "li":
		ensureNewline(output)
	}
}

func renderTable(output *bytes.Buffer, table *html.Node) {
	var rows [][]string
	var walk func(*html.Node)
	walk = func(node *html.Node) {
		if node.Type == html.ElementNode && strings.EqualFold(node.Data, "tr") {
			var cells []string
			for child := node.FirstChild; child != nil; child = child.NextSibling {
				if child.Type == html.ElementNode && (strings.EqualFold(child.Data, "th") || strings.EqualFold(child.Data, "td")) {
					value := strings.ReplaceAll(NodeText(child), "|", "\\|")
					cells = append(cells, value)
				}
			}
			if len(cells) > 0 {
				rows = append(rows, cells)
			}
			return
		}
		for child := node.FirstChild; child != nil; child = child.NextSibling {
			walk(child)
		}
	}
	walk(table)
	if len(rows) == 0 {
		return
	}
	columns := 0
	for _, row := range rows {
		columns = max(columns, len(row))
	}
	writeRow := func(row []string) {
		output.WriteString("| ")
		for column := range columns {
			if column < len(row) {
				output.WriteString(row[column])
			}
			output.WriteString(" | ")
		}
		output.WriteByte('\n')
	}
	ensureBlankLine(output)
	writeRow(rows[0])
	separator := make([]string, columns)
	for index := range separator {
		separator[index] = "---"
	}
	writeRow(separator)
	for _, row := range rows[1:] {
		writeRow(row)
	}
}

// Attribute returns the value of a case-insensitive attribute.
func Attribute(node *html.Node, name string) string {
	for _, attribute := range node.Attr {
		if strings.EqualFold(attribute.Key, name) {
			return attribute.Val
		}
	}
	return ""
}

// NodeText returns whitespace-normalized text content of a node.
func NodeText(node *html.Node) string {
	var output strings.Builder
	var walk func(*html.Node)
	walk = func(current *html.Node) {
		if current.Type == html.TextNode {
			output.WriteString(current.Data)
		}
		if current.Type == html.ElementNode && (current.Data == "script" || current.Data == "style") {
			return
		}
		separated := current.Type == html.ElementNode && blockText[current.Data]
		if separated {
			output.WriteByte(' ')
		}
		for child := current.FirstChild; child != nil; child = child.NextSibling {
			walk(child)
		}
		if separated {
			output.WriteByte(' ')
		}
	}
	walk(node)
	return strings.Join(strings.Fields(output.String()), " ")
}

// EscapeMarkdown escapes characters that would break a Markdown link label.
func EscapeMarkdown(value string) string {
	return strings.NewReplacer("\\", "\\\\", "[", "\\[", "]", "\\]").Replace(value)
}

func ensureNewline(output *bytes.Buffer) {
	if output.Len() > 0 && output.Bytes()[output.Len()-1] != '\n' {
		output.WriteByte('\n')
	}
}

func ensureBlankLine(output *bytes.Buffer) {
	if output.Len() == 0 {
		return
	}
	data := output.Bytes()
	if len(data) >= 2 && data[len(data)-2] == '\n' && data[len(data)-1] == '\n' {
		return
	}
	if data[len(data)-1] == '\n' {
		output.WriteByte('\n')
	} else {
		output.WriteString("\n\n")
	}
}
