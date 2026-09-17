// Package sources exposes the editable dataset sources (custom GitHub
// repositories and curated web page lists) to the dashboard.
package sources

import (
	"errors"
	"fmt"
	"net/url"
	"sort"

	"github.com/lkarlslund/knowledge-mcp/internal/model"
	"github.com/lkarlslund/knowledge-mcp/internal/provider/gitdocs"
	"github.com/lkarlslund/knowledge-mcp/internal/provider/webpages"
)

const (
	KindRepos    = "gitdocs"
	KindPageList = "webpages"
)

type Manager struct {
	repos *gitdocs.GitDocs
	pages *webpages.WebPages
}

func New(repos *gitdocs.GitDocs, pages *webpages.WebPages) *Manager {
	return &Manager{repos: repos, pages: pages}
}

func (m *Manager) ListSources() (model.SourceList, error) {
	result := model.SourceList{Repos: []model.RepoSource{}, PageLists: []model.PageListSource{}}
	for _, item := range m.repos.Catalog() {
		result.Repos = append(result.Repos, model.RepoSource{ID: item.ID, Repo: item.Repo, Branch: item.Branch, Name: item.Name, Description: item.Description, Include: item.Include, Custom: item.Custom})
	}
	if _, err := m.repos.CustomCatalog(); err != nil {
		result.RepoFileError = err.Error()
	}
	lists, err := m.pages.Sources()
	if err != nil {
		return result, err
	}
	for _, list := range lists {
		entry := model.PageListSource{ID: list.ID, File: list.File, Name: list.Config.Name, Description: list.Config.Description, Language: list.Config.Language, Pages: len(list.Config.Pages)}
		if list.Err != nil {
			entry.Error = list.Err.Error()
		}
		entry.Sites = sites(list.Config.Pages)
		result.PageLists = append(result.PageLists, entry)
	}
	return result, nil
}

func sites(pages []webpages.Page) []string {
	counts := map[string]int{}
	for _, page := range pages {
		if parsed, err := url.Parse(page.URL); err == nil && parsed.Host != "" {
			counts[parsed.Host]++
		}
	}
	hosts := make([]string, 0, len(counts))
	for host := range counts {
		hosts = append(hosts, host)
	}
	sort.Slice(hosts, func(i, j int) bool {
		if counts[hosts[i]] != counts[hosts[j]] {
			return counts[hosts[i]] > counts[hosts[j]]
		}
		return hosts[i] < hosts[j]
	})
	return hosts
}

// ReadSource returns the raw file. The repository kind has a single custom
// catalog file, so its ID is ignored.
func (m *Manager) ReadSource(kind, id string) (string, error) {
	switch kind {
	case KindRepos:
		content, err := m.repos.CustomCatalog()
		if content == "" && err == nil {
			content = "datasets:\n"
		}
		// A broken file is still returned so it can be fixed.
		return content, nil
	case KindPageList:
		return m.pages.ReadSource(id)
	}
	return "", unknownKind(kind)
}

func (m *Manager) SaveSource(kind, id, content string) error {
	switch kind {
	case KindRepos:
		return m.repos.SaveCustomCatalog(content)
	case KindPageList:
		return m.pages.SaveSource(id, content)
	}
	return unknownKind(kind)
}

func (m *Manager) DeleteSource(kind, id string) error {
	switch kind {
	case KindRepos:
		return errors.New("repositories are removed by editing the custom repository list")
	case KindPageList:
		return m.pages.DeleteSource(id)
	}
	return unknownKind(kind)
}

func unknownKind(kind string) error {
	return fmt.Errorf("unknown source kind %q", kind)
}
