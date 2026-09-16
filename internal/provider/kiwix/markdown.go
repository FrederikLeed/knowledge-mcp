package kiwix

import (
	"bytes"
	"fmt"
	"net/url"
	"path"
	"strings"

	"github.com/lkarlslund/knowledge-mcp/internal/htmlmarkdown"
	"golang.org/x/net/html"
)

func htmlMarkdown(source []byte, dataset string, namespace byte, entryPath string) string {
	return htmlmarkdown.Convert(source, htmlmarkdown.Options{Link: func(href string) string {
		return markdownLink(dataset, namespace, entryPath, href)
	}})
}

func markdownLink(dataset string, namespace byte, entryPath, href string) string {
	href = strings.TrimSpace(href)
	if href == "" || strings.HasPrefix(href, "#") {
		return href
	}
	parsed, err := url.Parse(href)
	if err != nil || parsed.IsAbs() || strings.HasPrefix(href, "//") || strings.HasPrefix(href, "mailto:") {
		return href
	}
	target := parsed.Path
	if !strings.HasPrefix(target, "/") {
		target = path.Join(path.Dir(entryPath), target)
	}
	target = strings.TrimPrefix(path.Clean(target), "/")
	if target == "." || target == "" {
		return href
	}
	id := zimDocumentID(namespace, target)
	result := fmt.Sprintf("knowledge-read://read?dataset=%s&id=%s", url.QueryEscape(dataset), id)
	if parsed.Fragment != "" {
		result += "&section=" + url.QueryEscape(parsed.Fragment)
	}
	return result
}

func nodeTextFromHTML(source []byte) string {
	document, err := html.Parse(bytes.NewReader(source))
	if err != nil {
		return string(source)
	}
	return htmlmarkdown.NodeText(document)
}
