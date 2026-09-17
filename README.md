# Knowledge MCP

A local-first MCP server for downloading, indexing, searching, and reading large
reference datasets. Providers own discovery, variants, acquisition, raw-record
access, and on-demand conversion. The core owns background jobs, atomic dataset
generations, local indexes, ranking, MCP, and the dashboard.

Included providers:

- `wikimedia`: official monthly Wikimedia current-content XML exports.
- `rfc`: the complete RFC Series from the RFC Editor, with a `text` variant.
- `kiwix`: the complete Kiwix OPDS catalog, grouped into datasets and archive flavours.
- `ncbi`: the PubMed annual baseline plus ordered daily additions, revisions, and deletions.
- `eurlex`: official EU legal acts currently in force, in any of the 24 official EU languages.
- `gitdocs`: Markdown documentation from GitHub repositories (Microsoft Learn, Home Assistant, AD security tooling), one dataset per repository.
- `webpages`: curated lists of web pages defined in local YAML/JSON files, converted to Markdown.

Provider URLs and local paths are not part of the agent contract. Search returns
a temporary opaque `ref`; agents pass only that value to `knowledge_read`, without
retaining provider or dataset routing details. References use ordinary fast random
identifiers, persist across service restarts, and expire after seven inactive days.
Markdown is the default read format, with embedded links carrying another opaque
reference that an agent can follow with `knowledge_read`.

![Knowledge MCP dashboard in dark mode](docs/dashboard-dark.png)

## Build and run

Go 1.25 or newer is required.

```sh
go build -o knowledge-mcp .
./knowledge-mcp serve
```

The server listens on `http://127.0.0.1:8765`; its MCP endpoint is
`http://127.0.0.1:8765/mcp` and its dashboard is
`http://127.0.0.1:8765/`. Runtime data defaults to `./data`.

```sh
./knowledge-mcp serve \
  --listen 127.0.0.1:9000 \
  --data-dir /srv/knowledge-data \
  --download-workers 3 \
  --index-workers 1 \
  --download-connections 3
```

Downloads and indexing use independent worker pools. One index job runs by
default to avoid competing Bleve segment merges; each job still uses the
configured internal decompression parallelism. Jobs submit and return
immediately; poll their IDs for status. Partial provider downloads are preserved
and resume after restarts. Updates are built in staging and atomically replace
the installed generation only after both its title index and shared-search
generation are ready.

The listener intentionally requires an explicit loopback IP and has no
authentication layer. `--allow-non-loopback` lifts the loopback restriction (an
explicit IP such as `0.0.0.0` is still required) for private container networks,
for example a sidecar that other containers reach by name. The server prints a
warning at startup: anyone who can reach the port can download, update, and
delete datasets and change settings. Runtime profiles are never served on a
non-loopback listener.

Runtime profiles are available only on that loopback listener under
`/debug/pprof/`, for example `go tool pprof http://127.0.0.1:8765/debug/pprof/profile?seconds=30`.

## CLI

All non-server commands call the running MCP backend once.

```sh
# Discover datasets from all providers.
./knowledge-mcp dataset available
./knowledge-mcp dataset available rfc
./knowledge-mcp dataset available devdocs
./knowledge-mcp dataset available pubmed

# Download the RFC text variant, then poll/control its jobs.
./knowledge-mcp dataset download rfc --variant text
./knowledge-mcp dataset download pubmed --variant baseline
./knowledge-mcp dataset download eurlex-in-force --variant en
./knowledge-mcp dataset status JOB_ID
./knowledge-mcp dataset status --dataset rfc
./knowledge-mcp dataset job JOB_ID --action pause
./knowledge-mcp dataset job JOB_ID --action resume
./knowledge-mcp dataset job JOB_ID --action cancel
./knowledge-mcp dataset job JOB_ID --action retry

# Installed datasets, updates, search, and reads.
./knowledge-mcp dataset list
./knowledge-mcp dataset update rfc
./knowledge-mcp search rfc "HTTP status codes"
./knowledge-mcp search rfc "RFC 9110" --mode full_text
./knowledge-mcp read rfc --id 9110

# GitHub documentation and curated web pages.
./knowledge-mcp dataset available gitdocs
./knowledge-mcp dataset download microsoftdocs-powershell-docs
./knowledge-mcp search microsoftdocs-powershell-docs "splatting"
./knowledge-mcp dataset download dbu-rules      # needs data/webpages/dbu-rules.yaml

# Wikimedia remains a provider, not a special core concept.
./knowledge-mcp dataset download dawiki --variant content-current
./knowledge-mcp search dawiki "Copenhagen architecture"
./knowledge-mcp read dawiki --id 12345
```

Set `KNOWLEDGE_MCP_SERVER` or pass `--server` for a non-default endpoint.

## MCP tools

| Tool | Purpose |
| --- | --- |
| `knowledge_list_available` | Discover datasets, providers, and variants. |
| `knowledge_list_local` | List installed datasets with explicit search availability and `title`/`full_text` capabilities. |
| `knowledge_status` | Inspect worker settings, provider health, and update scheduling. |
| `knowledge_download` | Submit a first-time background download. |
| `knowledge_update` | Submit an atomic background update or finish indexing. |
| `knowledge_job_status` | Poll one job by `job_id` or `dataset`. |
| `knowledge_job` | `pause`, `resume`, `cancel`, or `retry` a job. |
| `knowledge_search` | Search one local dataset, or omit `dataset` to search the shared corpus of all ready datasets; supports publication-date bounds and newest/oldest sorting and returns opaque references. |
| `knowledge_read` | Read by opaque `ref`; `dataset` plus `id` or exact title remains supported for compatibility. |

`knowledge_read` always returns Markdown. Large documents return `offset`,
`returned_chars`, `total_chars`, `truncated`, and `next_offset` for continuation.

Wikimedia reads preserve headings, lists, tables, links, infobox fields,
citations, redirects, outlines, and bounded references. RFC reads add lifecycle
status, structured relationships, section outlines and targeted section reads,
and convert RFC references into followable Markdown links. Kiwix reads ZIM files natively and converts stored HTML,
including tables and internal links, to Markdown.
PubMed reads preserve citation metadata, abstracts, publication types, identifiers,
MeSH terms, and available creation/publication/revision dates. EUR-Lex reads convert official XHTML and tables to Markdown and
turn CELEX cross-references into followable knowledge links.

## Provider architecture

The core contract is in `internal/provider`. Implementations are isolated:

```text
internal/provider/
├── provider.go          discovery, acquisition, and common corpus boundary
├── eurlex/              CELLAR discovery, EU legal XHTML, and Markdown
├── gitdocs/             GitHub repository catalog, tarball streaming, Markdown
├── kiwix/               OPDS catalog, native ZIM reader, HTML-to-Markdown
├── ncbi/                PubMed baseline + daily updates, XML, and Markdown
├── rfc/                 RFC Editor catalog, raw text, and Markdown
├── webpages/            Curated page lists, polite fetching, HTML-to-Markdown
└── wikimedia/           Current-content discovery, XML, and Markdown
```

Shared helpers live in `internal/htmlmarkdown` (HTML-to-Markdown, used by Kiwix
and webpages) and `internal/markdowndoc` (front matter, outlines, sections,
paging).

A provider supplies:

- a stable provider ID and ownership of dataset IDs;
- discovery metadata and one or more variants;
- current release metadata and a release fingerprint;
- resumable acquisition into a staging generation;
- resumable title/body record scans through the common corpus interface;
- provider-owned descriptions, scope, topics, cadence, and source features;
- stable identifiers, aliases, keywords, lifecycle status, temporal metadata, and rank weight; and
- on-demand conversion to Markdown.

Providers never build or query search indexes. The provider-neutral
`internal/knowledgeindex` package consumes the common corpus, owns the single
Bleve schema and ranking pipeline, and checkpoints only at provider-declared
safe cursors. Title and body builds therefore resume after service restarts for
every provider without provider-specific index formats.

The store sees datasets only. Manifests persist `provider`, `dataset`, and
`variant`; jobs and MCP responses use the same vocabulary. Existing installations
using the old `data/wikis` directory and legacy `wiki` manifest/job fields are
migrated when the server starts. Local generations live at
`data/datasets/<provider>/<dataset>` so provider-owned data cannot collide.

The RFC provider uses the official RFC Editor XML catalog and canonical text
documents. RFC numbers are its stable document IDs. Existing raw RFC files are
hard-linked or copied into an update generation, while new documents are
downloaded in parallel. Individual partial files use HTTP ranges when resumed.

The Kiwix provider caches and searches the complete OPDS catalog, resolves
Metalink mirrors, resumes partial ZIM downloads with byte ranges, verifies
SHA-256, and supports uncompressed, zlib, bzip2, XZ, and Zstandard clusters.
Zstandard decoding uses Klaus Post's optimized Go implementation.

The NCBI provider applies the official annual PubMed baseline followed by daily
update files in numeric order, including replacement records and deletion
tombstones. Acquisition reuses unchanged compressed parts, resumes new parts with
HTTP ranges, and verifies NCBI's published MD5 for each completed part. The EUR-Lex provider discovers in-force sector 3 legal acts
through the Publications Office CELLAR endpoint and stores the selected official
language as resumable XHTML documents. Updates for both providers are built as
new staging generations and reuse unchanged local source files.

### GitHub documentation (`gitdocs`)

The built-in catalog is `internal/provider/gitdocs/catalog.yaml`; each entry is
one dataset whose ID is the lowercased `owner-repo` (for example
`microsoftdocs-entra-docs`, `home-assistant-developers.home-assistant`,
`specterops-bloodhound`). It covers Microsoft Learn (Entra, Windows Server,
Defender/Sentinel, support articles, PowerShell 7.6, Microsoft 365, Azure,
security/privileged access), Home Assistant user and developer docs, and AD/Entra
security tools (BloodHound, SharpHound, AzureHound, PingCastle, AD Miner,
Maester, Certipy, PasswordSolution, Entra CA Insight, Azure tiering, GPOHound,
Certify, PSPKIAudit, Rubeus, DSInternals, CISA ScubaGear baselines, and an AD/infrastructure security knowledge base
(`ad-security-kb`, from The-Hacker-Recipes repository)). To add a repository without rebuilding, put entries in the custom
catalog `<data-dir>/gitdocs.yaml` (`--gitdocs-catalog`), most easily from the
dashboard's **Sources** section. It uses the same format, is re-read whenever it
changes, and may not reuse a built-in dataset ID; a broken file is reported and
the built-in catalog stays available. Per entry you set:

- `repo`, optional `branch` (default: the default branch HEAD), name, description, topics;
- `include` / `exclude` path globs (`**` spans directories);
- `fragments`: include-only snippets, stored for `[!INCLUDE]` expansion but not indexed;
- `url_rules`: ordered prefix-to-URL rules for citations (a `*` prefix segment
  matches one directory; `lowercase`, `trailing_slash`, and `query` adjust the
  URL). Paths without a rule cite the GitHub blob URL at the indexed commit.

The release is the branch head commit, resolved through the GitHub REST API
(`GITHUB_TOKEN` is used when set) with a fallback to the anonymous git ref
advertisement when the API is rate limited. Acquisition streams
`codeload.github.com/<repo>/tar.gz/<sha>` and writes only matching Markdown
(`.md`, `.markdown`, `.mdx`) and DocFX YAML files, so image and media trees are
never stored (Azure docs included). YAML files are kept only when they carry a
`### YamlMime:` content header other than landing/hub/TOC pages, and are
flattened to Markdown. Titles come from front matter `title:`, then the first
`# ` heading, then the file name. Front matter is removed from bodies; fields such
as `description`, `ms.date`, `ms.topic`, and `ms.service` become record metadata
and keywords (`ms.date` becomes the modification date). Relative links to other
indexed files become followable knowledge links; other relative links point to
GitHub. Reads return a short source header and support `section` and outlines.
An update re-streams the archive for the new commit.

### Curated web pages (`webpages`)

Each `*.yaml`, `*.yml`, or `*.json` file in `--webpages-dir` (default
`<data-dir>/webpages`) is one dataset named after the file. Choose names that
no other provider uses. The file lists pages and optional fetch settings:

```yaml
name: DBU love og regler
description: Danish football rules and regulations.
language: da            # en (default) or da
topics: [football, rules]
concurrency: 2          # parallel fetches (max 4)
delay: 1s               # pause after each fetch per worker
user_agent: ""          # default: a browser-like agent
pages:
  - {slug: jylland-turneringsreglement, title: "DBU Jyllands Turneringsreglement", url: "https://www.dbujylland.dk/..."}
  - {slug: disciplinaere-bestemmelser, title: "De disciplinære bestemmelser", url: "https://www.dbu.dk/media/.../x.pdf", type: pdf}
  - {slug: herre-dm-regler, title: "Herre-DM regler", url: "https://divisionsforeningen.dk/love-og-regler", type: pdf, pdf_link_text: "Turneringsregler for Herre-DM"}
```

PDF entries (and linked PDF assets) are downloaded and their text is extracted
with poppler's `pdftotext` (installed in the container image); numbered `§`
paragraphs and chapter headings become Markdown sections. Without `pdftotext`
a PDF is indexed by title and link only.

A list can also discover pages itself. Each `crawl` entry follows links from
its `start` pages and `sitemaps`, fetching only URLs under an `include`
prefix, skipping URLs containing an `exclude` string and anything robots.txt
disallows. Linked PDFs under `include`, and links under a `documents` prefix
(for CMS asset URLs without a `.pdf` extension), are indexed as PDFs and not
followed. `max_pages` (default 1000, at most 10000) and `max_depth` (default 6)
bound the crawl. Crawled pages are fetched once per weekly release; statically
listed pages keep their slug and title.

```yaml
crawl:
  - start: ["https://www.dbu.dk/turneringer-og-resultater/love-og-regler/"]
    include: ["https://www.dbu.dk/turneringer-og-resultater/love-og-regler/"]
    documents: ["https://www.dbu.dk/media/"]
    max_pages: 2000
```

Page lists can also be created, edited, and deleted in the dashboard's
**Sources** section, which validates each file before saving it and can start
the fetch right away. Deleting a list file keeps downloaded data until the
dataset itself is deleted.

`contrib/webpages/dbu-rules.yaml` covers DBU rules: 493 curated pages plus crawls of
every rules section on dbu.dk, divisionsforeningen.dk and the six lokalunion
sites
(the container image ships it under `/usr/share/knowledge-mcp/webpages`); copy
it into the webpages directory to enable the `dbu-rules` dataset.
`contrib/webpages/ad-security-references.yaml` (shipped the same way) lists 85
AD/Entra security reference pages: MITRE ATT&CK technique pages, Microsoft
security policy, Graph permission and KB references not covered by gitdocs,
NIST SP 800-63B, and published AD security research; copy it to enable
the `ad-security-references` dataset.

Slugs are the document IDs. HTML pages are stored raw and converted to Markdown
on indexing and reads: the `main`/`article` element is used, navigation,
breadcrumbs, share widgets, forms, and footers are dropped, tables stay tables,
and links become absolute. PDF text is not extracted: `type: pdf` entries are
indexed by title with a link to the document; with `pdf_link_text` the current
asset link is resolved from the landing page on every fetch. The release
fingerprint combines the page list with the ISO week, so an edited list or a new
week refetches every page. Failed pages (HTTP errors after retries for 429/5xx)
never abort the run: they are recorded in `documents.json`, an update keeps the
previous copy marked as stale, and the job fails only when no page could be
fetched.

## Container

The `Dockerfile` builds a static binary into a small Alpine image that runs as
an unprivileged user with `/data` as a volume. Its default command is
`serve --listen 0.0.0.0:8765 --allow-non-loopback --data-dir /data`.

```sh
docker build -t knowledge-mcp .
docker network create knowledge
docker run -d --name knowledge-mcp --network knowledge -v knowledge-data:/data knowledge-mcp
# CLI from another container on the same private network:
docker run --rm --network knowledge knowledge-mcp --server http://knowledge-mcp:8765/mcp dataset list
```

Other containers on the network use `http://knowledge-mcp:8765/mcp`. Do not
publish the port beyond loopback (`-p 127.0.0.1:8765:8765`).

## Dashboard

The embedded Bootstrap/Alpine.js dashboard shows local datasets, provider and
variant metadata, document counts, storage attribution, upgrades, and live job
progress. The complete provider catalog is searchable in a modal, with language
and installed-state filters. Dataset variants are selected before download.
Updates arrive over a WebSocket; no CDN is required. The gear menu changes
download/index worker limits at runtime, controls per-index decompression
parallelism, configures periodic update checks and optional automatic updates,
and reports provider catalog health. Provider catalogs persist as atomic snapshots
under `data/catalogs`, refresh automatically when the configured update interval
expires, can be refreshed immediately from Settings, and remain usable while an
upstream catalog is temporarily unavailable. Settings persist in `data/settings.json`.

## User service

```sh
mkdir -p ~/.local/lib/knowledge-mcp \
  ~/.local/share/knowledge-mcp/data \
  ~/.config/systemd/user
go build -o ~/.local/lib/knowledge-mcp/knowledge-mcp .
cp contrib/systemd/knowledge-mcp.service ~/.config/systemd/user/
systemctl --user daemon-reload
systemctl --user enable --now knowledge-mcp.service
```

Inspect it with:

```sh
systemctl --user status knowledge-mcp.service
journalctl --user -u knowledge-mcp.service
```

An agent installing a release should download the matching binary and `.sha256`
asset from the latest GitHub release, verify it with `sha256sum -c`, install the
binary and included user unit at the paths above, start the service, verify
`http://127.0.0.1:8765/healthz`, and register the exact Streamable HTTP MCP URL
`http://127.0.0.1:8765/mcp`. Stdio-only clients can run:

```sh
~/.local/lib/knowledge-mcp/knowledge-mcp mcp stdio
```

Daily rolling releases provide Linux, macOS, and Windows binaries for AMD64 and
ARM64, with SHA-256 checksums.

## Storage and indexing

Runtime data is not committed. Each installed dataset has a provider-owned raw
area, provider metadata, a compact lookup/title index, and a manifest. Searchable
documents from every provider feed one logical shared BM25 corpus, uniformly
striped across physical Bleve shards. Body text is indexed but not duplicated as
a stored field. Wikimedia reads open only the page-range bzip2 source file recorded
for the selected page;
RFC reads open the canonical local Markdown source; Kiwix reads only the referenced
ZIM cluster; PubMed reads one compressed baseline or update part; EUR-Lex reads one local
XHTML document; gitdocs reads one Markdown file (plus its includes); and webpages
reads one stored HTML page.

An initial install exposes title lookup while shared indexing continues. An
update builds a hidden shared generation and atomically publishes its provider
data, title lookup, and search generation when all are ready. Searches are always
filtered by the durable active-generation manifest, so retired or partially built
documents cannot leak into results. Removing a dataset first removes its generation
from that manifest; a managed background cleanup then reclaims its index terms.
Interrupted builds resume from provider-declared safe cursors. Search supports
`auto`, `title`, and `full_text` modes, with shared BM25 body relevance plus generic
title, alias, keyword, exact-identifier, lifecycle, and provider rank signals.
Providers also expose dates when their sources define them: RFC publication dates,
Wikimedia revision timestamps, PubMed creation/publication/revision dates, and
EUR-Lex document dates. `knowledge_search` accepts `published_after`,
`published_before`, and `sort` (`relevance`, `newest`, or `oldest`).

## License

Knowledge MCP is released under the [MIT License](LICENSE).
