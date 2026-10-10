// Copyright 2025
// SPDX-License-Identifier: Apache-2.0

package smartdl

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"path/filepath"
	"strings"
	"time"

	"github.com/bashrusakh/hfdesk/internal/hubtree"
)

const defaultEndpoint = "https://huggingface.co"

// AnalyzerOptions configures the Analyzer.
type AnalyzerOptions struct {
	// Token is the HuggingFace access token for private repos.
	Token string

	// Endpoint is the HuggingFace Hub base URL (default: https://huggingface.co).
	Endpoint string

	// HTTPClient is an optional custom HTTP client.
	HTTPClient *http.Client
}

// Analyzer analyzes HuggingFace repositories to determine their type and structure.
type Analyzer struct {
	token    string
	endpoint string
	client   *http.Client
}

// NewAnalyzer creates a new Analyzer with the given options.
func NewAnalyzer(opts AnalyzerOptions) *Analyzer {
	endpoint := opts.Endpoint
	if endpoint == "" {
		endpoint = defaultEndpoint
	}
	endpoint = strings.TrimSuffix(endpoint, "/")

	client := opts.HTTPClient
	if client == nil {
		client = &http.Client{
			Timeout: 30 * time.Second,
			Transport: &http.Transport{
				MaxIdleConns:        10,
				IdleConnTimeout:     90 * time.Second,
				TLSHandshakeTimeout: 10 * time.Second,
			},
		}
	}

	return &Analyzer{
		token:    opts.Token,
		endpoint: endpoint,
		client:   client,
	}
}

// Analyze fetches and analyzes a HuggingFace repository using the default "main" revision.
// If isDataset is false, it will first try to fetch as a model, then as a dataset if not found.
func (a *Analyzer) Analyze(ctx context.Context, repo string, isDataset bool) (*RepoInfo, error) {
	return a.AnalyzeWithRevision(ctx, repo, isDataset, "main")
}

// AnalyzeWithRevision fetches and analyzes a HuggingFace repository at a specific revision.
// If isDataset is false, it will first try to fetch as a model, then as a dataset if not found.
func (a *Analyzer) AnalyzeWithRevision(ctx context.Context, repo string, isDataset bool, revision string) (*RepoInfo, error) {
	if revision == "" {
		revision = "main"
	}

	// Fetch file tree - auto-detect model vs dataset if not explicitly a dataset
	files, detectedIsDataset, commit, err := a.fetchFileTreeAutoDetect(ctx, repo, isDataset, revision)
	if err != nil {
		return nil, fmt.Errorf("fetch file tree: %w", err)
	}
	isDataset = detectedIsDataset

	// Build RepoInfo
	info := &RepoInfo{
		Repo:       repo,
		IsDataset:  isDataset,
		Files:      files,
		FileCount:  len(files),
		Commit:     commit,
		Branch:     revision,
		AnalyzedAt: time.Now().UTC(),
		Metadata:   make(map[string]interface{}),
	}

	// Calculate total size
	for _, f := range files {
		info.TotalSize += f.Size
	}
	info.TotalSizeHuman = humanSize(info.TotalSize)

	// Detect type
	info.Type = a.detectType(files, isDataset)
	info.TypeDescription = info.Type.Description()

	// Fetch refs (branches/tags) - non-fatal if fails
	if refs, err := a.fetchRefs(ctx, repo, isDataset); err == nil {
		info.Refs = refs
		// Fallback: if the tree API didn't return X-Repo-Commit, resolve the
		// commit by matching the requested revision against a branch/tag.
		if info.Commit == "" {
			for _, ref := range refs {
				if ref.Name == revision && ref.Commit != "" {
					info.Commit = ref.Commit
					break
				}
			}
		}
	}

	// Fetch and parse metadata files based on detected type
	if err := a.fetchMetadata(ctx, repo, isDataset, info); err != nil {
		return nil, err
	}

	// Run type-specific analysis
	a.analyzeTypeSpecific(info)

	// For GGUF repos: build the model-provenance chain and, when no local
	// mmproj files exist, search upstream repos for them.
	if info.Type == TypeGGUF && info.GGUF != nil {
		a.buildModelChain(ctx, info)
	}

	// Populate SelectableItems based on type
	populateSelectableItems(info)

	// Populate web/API download defaults.
	info.PopulateDownloadSelection()

	return info, nil
}

// hfTreeNode represents a node in the HF tree API response.
type hfTreeNode struct {
	Type string `json:"type"` // "file" or "directory"
	Path string `json:"path"`
	Size int64  `json:"size,omitempty"`
	LFS  *struct {
		Size   int64  `json:"size,omitempty"`
		SHA256 string `json:"sha256,omitempty"`
		OID    string `json:"oid,omitempty"`
	} `json:"lfs,omitempty"`
}

// ErrBothExist is returned when a repo exists as both model and dataset.
var ErrBothExist = fmt.Errorf("repository exists as both model and dataset")

// fetchFileTreeAutoDetect tries to fetch as model first, then as dataset if 404.
// Returns the files, whether it's a dataset, the resolved commit SHA, and any
// error. If both model and dataset exist, returns ErrBothExist.
func (a *Analyzer) fetchFileTreeAutoDetect(ctx context.Context, repo string, isDataset bool, revision string) ([]FileInfo, bool, string, error) {
	// If explicitly marked as dataset, fetch as dataset directly
	if isDataset {
		files, commit, err := a.fetchFileTree(ctx, repo, true, revision)
		return files, true, commit, err
	}

	// Try as model first
	modelFiles, modelCommit, modelErr := a.fetchFileTree(ctx, repo, false, revision)

	// Helper to check if error indicates repo doesn't exist as model
	// HuggingFace returns "not found" for missing repos, but also "unauthorized"
	// when trying to access a datasets-only repo via the models API
	isModelNotFound := func(err error) bool {
		// Structural pagination failure means an incomplete selected tree,
		// not a missing namespace, regardless of URL/header diagnostic text.
		if err == nil || errors.Is(err, hubtree.ErrPagination) {
			return false
		}
		errStr := strings.ToLower(err.Error())
		return strings.Contains(errStr, "not found") ||
			strings.Contains(errStr, "unauthorized") ||
			strings.Contains(errStr, "401")
	}

	// If model not found or unauthorized, try as dataset
	if isModelNotFound(modelErr) {
		datasetFiles, datasetCommit, datasetErr := a.fetchFileTree(ctx, repo, true, revision)
		if datasetErr == nil {
			return datasetFiles, true, datasetCommit, nil
		}
		// If dataset also fails with not found/unauthorized, return helpful error
		if isModelNotFound(datasetErr) {
			return nil, false, "", fmt.Errorf("repository not found as model or dataset: %s", repo)
		}
		// Dataset failed with different error (actual auth issue, network, etc.)
		return nil, false, "", datasetErr
	}

	// If model found, check if dataset also exists
	if modelErr == nil {
		_, _, datasetErr := a.fetchFileTree(ctx, repo, true, revision)
		if datasetErr == nil {
			// Both exist - return error so caller can ask user
			return nil, false, "", ErrBothExist
		}
		// Only model exists
		return modelFiles, false, modelCommit, nil
	}

	// Return original error for other failures (network, etc.)
	return nil, false, "", modelErr
}

// fetchFileTree recursively fetches the file tree from HuggingFace API.
// The second return value is the resolved commit SHA for the revision (from
// the X-Repo-Commit response header), or "" if the API did not provide it.
func (a *Analyzer) fetchFileTree(ctx context.Context, repo string, isDataset bool, revision string) ([]FileInfo, string, error) {
	var files []FileInfo
	var commit string
	err := a.walkTree(ctx, repo, isDataset, revision, "", &commit, func(node hfTreeNode) error {
		if node.Type == "file" || node.Type == "blob" {
			size := node.Size
			isLFS := false
			sha256 := ""
			if node.LFS != nil {
				size = node.LFS.Size
				isLFS = true
				sha256 = node.LFS.SHA256
				if sha256 == "" {
					sha256 = node.LFS.OID
				}
			}

			files = append(files, FileInfo{
				Path:      node.Path,
				Name:      filepath.Base(node.Path),
				Size:      size,
				SizeHuman: humanSize(size),
				IsLFS:     isLFS,
				SHA256:    sha256,
				Directory: filepath.Dir(node.Path),
			})
		}
		return nil
	})
	return files, commit, err
}

// walkTree walks the repository tree through the shared hubtree walker
// (issue #96): recursive=true with Link rel="next" pagination, per-directory
// fallback for mirrors that ignore recursive, bounded malformed listings,
// and bounded Retry-After/RateLimit-aware retries of 429/5xx/network errors.
// When commitOut is non-nil and still empty, the resolved commit SHA is
// captured from the X-Repo-Commit response header that HuggingFace returns
// for the requested revision — from the first response that carries it, as
// before. Terminal statuses fail immediately with the historical error
// messages below.
func (a *Analyzer) walkTree(ctx context.Context, repo string, isDataset bool, revision, prefix string, commitOut *string, fn func(hfTreeNode) error) error {
	w := &hubtree.Walker{
		TreeURL: func(p string) string {
			return a.treeURL(repo, isDataset, revision, p)
		},
		Token:     a.token,
		UserAgent: "hfdownloader/3",
		Client:    a.client,
		StatusErr: func(resp *http.Response) error {
			switch resp.StatusCode {
			case http.StatusUnauthorized:
				return fmt.Errorf("unauthorized: repo requires token or you do not have access")
			case http.StatusForbidden:
				return fmt.Errorf("forbidden: please accept the repository terms at %s", a.repoURL(repo, isDataset))
			case http.StatusNotFound:
				return fmt.Errorf("repository not found: %s", repo)
			default:
				return fmt.Errorf("API error: %s", resp.Status)
			}
		},
		DecodeErr: func(err error) error {
			return fmt.Errorf("decode response: %w", err)
		},
		OnResponse: func(resp *http.Response) {
			if commitOut != nil && *commitOut == "" {
				if c := resp.Header.Get("X-Repo-Commit"); c != "" {
					*commitOut = c
				}
			}
		},
	}
	return w.Walk(ctx, prefix, func(n hubtree.Node) error {
		return fn(toTreeNode(n))
	})
}

// toTreeNode converts a shared hubtree node to the analyzer's node type
// without losing any field.
func toTreeNode(n hubtree.Node) hfTreeNode {
	out := hfTreeNode{Type: n.Type, Path: n.Path, Size: n.Size}
	if n.LFS != nil {
		out.LFS = &struct {
			Size   int64  `json:"size,omitempty"`
			SHA256 string `json:"sha256,omitempty"`
			OID    string `json:"oid,omitempty"`
		}{Size: n.LFS.Size, SHA256: n.LFS.Sha256, OID: n.LFS.Oid}
	}
	return out
}

// treeURL builds the tree API URL.
func (a *Analyzer) treeURL(repo string, isDataset bool, revision, prefix string) string {
	var base string
	if isDataset {
		base = fmt.Sprintf("%s/api/datasets/%s/tree/%s", a.endpoint, repo, url.PathEscape(revision))
	} else {
		base = fmt.Sprintf("%s/api/models/%s/tree/%s", a.endpoint, repo, url.PathEscape(revision))
	}
	if prefix != "" {
		base += "/" + pathEscapeAll(prefix)
	}
	return base
}

// repoURL builds the repository page URL.
func (a *Analyzer) repoURL(repo string, isDataset bool) string {
	if isDataset {
		return fmt.Sprintf("%s/datasets/%s", a.endpoint, repo)
	}
	return fmt.Sprintf("%s/%s", a.endpoint, repo)
}

// rawURL builds the raw file URL for fetching content.
func (a *Analyzer) rawURL(repo string, isDataset bool, revision, path string) string {
	if isDataset {
		return fmt.Sprintf("%s/datasets/%s/raw/%s/%s", a.endpoint, repo, url.PathEscape(revision), pathEscapeAll(path))
	}
	return fmt.Sprintf("%s/%s/raw/%s/%s", a.endpoint, repo, url.PathEscape(revision), pathEscapeAll(path))
}

// resolveURL builds the LFS-resolving file URL. Git-LFS-backed files are stored
// as pointer stubs under /raw/; /resolve/ serves the actual bytes, matching the
// downloader's LFS routing (pkg/hfdownloader lfsURL).
func (a *Analyzer) resolveURL(repo string, isDataset bool, revision, path string) string {
	if isDataset {
		return fmt.Sprintf("%s/datasets/%s/resolve/%s/%s", a.endpoint, repo, url.PathEscape(revision), pathEscapeAll(path))
	}
	return fmt.Sprintf("%s/%s/resolve/%s/%s", a.endpoint, repo, url.PathEscape(revision), pathEscapeAll(path))
}

// hfRefsResponse represents the HuggingFace refs API response.
type hfRefsResponse struct {
	Branches []hfRef `json:"branches"`
	Tags     []hfRef `json:"tags"`
}

type hfRef struct {
	Name         string `json:"name"`
	Ref          string `json:"ref"`
	TargetCommit string `json:"targetCommit"`
}

// fetchRefs fetches available branches and tags from the repository.
func (a *Analyzer) fetchRefs(ctx context.Context, repo string, isDataset bool) ([]RepoRef, error) {
	var apiPath string
	if isDataset {
		apiPath = fmt.Sprintf("%s/api/datasets/%s/refs", a.endpoint, repo)
	} else {
		apiPath = fmt.Sprintf("%s/api/models/%s/refs", a.endpoint, repo)
	}

	req, err := http.NewRequestWithContext(ctx, "GET", apiPath, nil)
	if err != nil {
		return nil, err
	}
	a.addAuth(req)

	resp, err := a.client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	if resp.StatusCode != 200 {
		return nil, fmt.Errorf("fetch refs: %s", resp.Status)
	}

	var refsResp hfRefsResponse
	if err := json.NewDecoder(resp.Body).Decode(&refsResp); err != nil {
		return nil, fmt.Errorf("decode refs: %w", err)
	}

	var refs []RepoRef
	for _, b := range refsResp.Branches {
		refs = append(refs, RepoRef{
			Name:   b.Name,
			Type:   "branch",
			Commit: b.TargetCommit,
		})
	}
	for _, t := range refsResp.Tags {
		refs = append(refs, RepoRef{
			Name:   t.Name,
			Type:   "tag",
			Commit: t.TargetCommit,
		})
	}

	return refs, nil
}

// addAuth adds authentication headers to a request.
func (a *Analyzer) addAuth(req *http.Request) {
	if a.token != "" {
		req.Header.Set("Authorization", "Bearer "+a.token)
	}
	req.Header.Set("User-Agent", "hfdownloader/3")
}

// pathEscapeAll escapes each path segment.
func pathEscapeAll(p string) string {
	segs := strings.Split(p, "/")
	for i := range segs {
		segs[i] = url.PathEscape(segs[i])
	}
	return strings.Join(segs, "/")
}

// detectType determines the repository type based on files present.
func (a *Analyzer) detectType(files []FileInfo, isDataset bool) RepoType {
	if isDataset {
		return TypeDataset
	}

	// Build file index for quick lookups
	hasFile := make(map[string]bool)
	var extensions []string
	for _, f := range files {
		hasFile[f.Path] = true
		hasFile[f.Name] = true
		ext := strings.ToLower(filepath.Ext(f.Name))
		extensions = append(extensions, ext)
	}

	// Priority-based detection

	// 1. GGUF - presence of .gguf files
	for _, ext := range extensions {
		if ext == ".gguf" {
			return TypeGGUF
		}
	}

	// 2. Diffusers - model_index.json is the definitive marker
	if hasFile["model_index.json"] {
		return TypeDiffusers
	}

	// 3. LoRA/Adapter - adapter_config.json
	if hasFile["adapter_config.json"] {
		return TypeLoRA
	}

	// 4. GPTQ/AWQ - quantize_config.json
	if hasRootFile(files, "quantize_config.json") && hasRootWeights(files) {
		// Will refine to GPTQ vs AWQ when we parse the config
		return TypeGPTQ
	}

	// 5. Transformers - root config.json + root safetensors/bin
	if hasRootFile(files, "config.json") && hasRootWeights(files) {
		return TypeTransformers
	}

	// 6. ONNX - presence of .onnx files (if not already detected as other type)
	for _, ext := range extensions {
		if ext == ".onnx" {
			return TypeONNX
		}
	}

	return TypeGeneric
}

// hasRootFile matches a root filename against the exact repository path.
func hasRootFile(files []FileInfo, name string) bool {
	for _, f := range files {
		if f.Path == name {
			return true
		}
	}
	return false
}

// hasRootWeights excludes nested exports and tokenizer data from root model detection.
func hasRootWeights(files []FileInfo) bool {
	for _, f := range files {
		if strings.Contains(f.Path, "/") {
			continue
		}
		name := strings.ToLower(f.Path)
		if strings.HasSuffix(name, ".safetensors") || (strings.HasSuffix(name, ".bin") && !strings.Contains(name, "tokenizer")) {
			return true
		}
	}
	return false
}

// fetchMetadata fetches and parses relevant config files.
func (a *Analyzer) fetchMetadata(ctx context.Context, repo string, isDataset bool, info *RepoInfo) error {
	// Determine which files to fetch based on detected type
	var filesToFetch []string
	switch info.Type {
	case TypeGGUF:
		filesToFetch = []string{"config.json", "README.md"}
	case TypeDiffusers:
		filesToFetch = []string{"model_index.json"}
	case TypeLoRA:
		filesToFetch = []string{"adapter_config.json"}
	case TypeGPTQ, TypeAWQ:
		filesToFetch = []string{"quantize_config.json", "config.json"}
	case TypeTransformers:
		filesToFetch = []string{"config.json", "tokenizer_config.json", "generation_config.json", "preprocessor_config.json", "processor_config.json"}
	case TypeONNX:
		filesToFetch = []string{"config.json"}
	default:
		// For generic/undetected types, fetch all possible config files to help refine detection
		filesToFetch = []string{"config.json", "preprocessor_config.json", "processor_config.json"}
	}

	if !isDataset {
		filesToFetch = append(filesToFetch, "quantization_config.json")
	}
	for _, path := range filesToFetch {
		// Check if the file exists and whether it is Git-LFS-backed.
		found := false
		isLFS := false
		for _, f := range info.Files {
			if f.Path == path {
				found = true
				isLFS = f.IsLFS
				break
			}
		}
		if !found {
			continue
		}

		content, err := a.fetchFile(ctx, repo, isDataset, info.Branch, path, isLFS)
		if err != nil {
			if errors.Is(err, errMetadataTooLarge) && path == "quantization_config.json" {
				data, headErr := decodeQuantizationHead(content)
				if headErr == nil {
					info.Metadata[path] = data
					continue
				}
				return fmt.Errorf("metadata %s: %w: %v", path, err, headErr)
			}
			var readErr *metadataReadError
			if errors.Is(err, errMetadataTooLarge) || errors.As(err, &readErr) {
				return fmt.Errorf("metadata %s: %w", path, err)
			}
			continue // Non-fatal
		}

		// Parse JSON content
		var data interface{}
		if err := json.Unmarshal(content, &data); err != nil {
			continue
		}
		info.Metadata[path] = data
	}

	return nil
}

// fetchFile fetches file content from the repository. Git-LFS-backed files are
// fetched through /resolve/ so the actual bytes are returned instead of the
// pointer stub that /raw/ serves (issue #123). A pointer stub from a file the
// tree did not mark as LFS is retried through /resolve/ once; a pointer that
// persists is an explicit failure and is never interpreted as file content.
// Authentication keeps the default net/http redirect policy, which drops
// Authorization on the cross-host CDN hop /resolve/ introduces.
func (a *Analyzer) fetchFile(ctx context.Context, repo string, isDataset bool, revision, path string, isLFS bool) ([]byte, error) {
	reqURL := a.rawURL(repo, isDataset, revision, path)
	if isLFS {
		reqURL = a.resolveURL(repo, isDataset, revision, path)
	}

	content, err := a.fetchFileFrom(ctx, reqURL, path)
	if err != nil && !errors.Is(err, errMetadataTooLarge) {
		return nil, err
	}
	if isLFSPointer(content) && !isLFS {
		// Unmarked LFS file: /raw/ served the pointer stub, so the content is
		// known to be missing; failing to replace it must stay explicit.
		content, err = a.fetchFileFrom(ctx, a.resolveURL(repo, isDataset, revision, path), path)
		if err != nil && !errors.Is(err, errMetadataTooLarge) {
			return nil, &metadataReadError{fmt.Errorf("git-lfs pointer served instead of file content: %w", err)}
		}
	}
	if isLFSPointer(content) {
		return nil, &metadataReadError{errors.New("git-lfs pointer served instead of file content")}
	}
	return content, err
}

// fetchFileFrom reads one file's content under the metadata size bound.
func (a *Analyzer) fetchFileFrom(ctx context.Context, reqURL, path string) ([]byte, error) {
	req, err := http.NewRequestWithContext(ctx, "GET", reqURL, nil)
	if err != nil {
		return nil, err
	}
	a.addAuth(req)

	resp, err := a.client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	if resp.StatusCode != 200 {
		return nil, fmt.Errorf("fetch %s: %s", path, resp.Status)
	}

	content, err := io.ReadAll(io.LimitReader(resp.Body, maxMetadataSize+1))
	if err != nil {
		return nil, &metadataReadError{err}
	}
	if len(content) > maxMetadataSize {
		return content[:maxMetadataSize], errMetadataTooLarge
	}
	return content, nil
}

// isLFSPointer reports whether content is a Git LFS pointer stub (the pointer
// format of the Git LFS v1 spec) rather than actual file content.
func isLFSPointer(content []byte) bool {
	first, _, _ := bytes.Cut(content, []byte("\n"))
	return string(bytes.TrimSpace(first)) == "version https://git-lfs.github.com/spec/v1"
}

const maxMetadataSize = 10 * 1024 * 1024

var errMetadataTooLarge = errors.New("exceeds 10 MiB metadata limit")

type metadataReadError struct{ error }

func (e *metadataReadError) Unwrap() error { return e.error }

// partialHeadMarker marks a bounded head whose recovery stopped at the size
// bound: the config continued past the recovered fields, so precision
// declarations the tail may hold are unknown and uniform precision cannot be
// established from the head alone. It is stored in the recovered map (which
// replaces the config under Metadata) so the partial recovery is explicit in
// the response instead of silent.
const partialHeadMarker = "__partial__"

// precisionHeadKeys are the config declarations that carry bit-width or
// effective-precision evidence. A value cut at the size bound is recovered as
// an explicit null: the declaration happened, its width is unknown, and it
// must not be silently dropped or invented.
var precisionHeadKeys = map[string]bool{
	"bits":                   true,
	"head_bits":              true,
	"vision_bits":            true,
	"mtp_bits":               true,
	"expert_bits":            true,
	"routed_expert_bits":     true,
	"routed_expert_bits_avg": true,
	"bits_per_weight":        true,
}

// decodeQuantizationHead keeps only completed top-level fields useful for the
// quantization projection (quant_method and the precisionHeadKeys widths). An
// incomplete large trailing value is expected at the cap; malformed JSON is not
// recovery. A precision declaration whose value was cut at the cap is kept as
// an explicit null and the head is marked partial, so truncation never reads as
// a completed declaration.
func decodeQuantizationHead(content []byte) (map[string]interface{}, error) {
	d := json.NewDecoder(bytes.NewReader(content))
	token, err := d.Token()
	if err != nil || token != json.Delim('{') {
		return nil, errors.New("quantization head is not an object")
	}
	head := make(map[string]interface{})
	completed := 0
	for d.More() {
		token, err := d.Token()
		if err != nil {
			if errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF) {
				break
			}
			return nil, err
		}
		key, _ := token.(string)
		var value interface{}
		if err := d.Decode(&value); err != nil {
			if errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF) {
				// The key is known and its value was cut at the cap.
				if precisionHeadKeys[key] {
					head[key] = nil
				}
				break
			}
			return nil, err
		}
		// A number at the cutoff may still be a prefix (4 of 4.5).
		// Only a visible member delimiter proves the value is complete.
		tail := bytes.TrimLeft(content[d.InputOffset():], " \t\r\n")
		if len(tail) == 0 {
			if precisionHeadKeys[key] {
				head[key] = nil
			}
			break
		}
		if tail[0] != ',' && tail[0] != '}' {
			return nil, errors.New("invalid quantization field delimiter")
		}
		switch key {
		case "quant_method":
			if method, ok := value.(string); ok && method != "" {
				head["quant_method"] = method
				completed++
			}
		case "bits", "head_bits", "vision_bits", "mtp_bits", "bits_per_weight", "routed_expert_bits_avg":
			if _, ok := value.(float64); ok {
				head[key] = value
				completed++
			}
		case "expert_bits", "routed_expert_bits":
			// Per-expert widths: an object keyed by expert or a single width.
			switch value.(type) {
			case map[string]interface{}, float64:
				head[key] = value
				completed++
			}
		}
	}
	partial := true
	if token, err := d.Token(); err == nil && token == json.Delim('}') {
		if len(bytes.TrimSpace(content[d.InputOffset():])) != 0 {
			return nil, errors.New("invalid trailing quantization data")
		}
		partial = false
	}
	if completed == 0 {
		return nil, errors.New("no completed quantization fields in bounded head")
	}
	if partial {
		head[partialHeadMarker] = true
	}
	return head, nil
}

// analyzeTypeSpecific runs type-specific analysis.
func (a *Analyzer) analyzeTypeSpecific(info *RepoInfo) {
	switch info.Type {
	case TypeGGUF:
		info.GGUF = analyzeGGUF(info.Files)
	case TypeDiffusers:
		info.Diffusers = analyzeDiffusers(info.Files, info.Metadata)
	case TypeLoRA:
		info.LoRA = analyzeLoRA(info.Metadata)
	case TypeGPTQ, TypeAWQ:
		info.Quantized = analyzeQuantized(info.Metadata)
		// Refine type based on actual method
		if info.Quantized != nil && info.Quantized.Method == "awq" {
			info.Type = TypeAWQ
			info.TypeDescription = info.Type.Description()
		}
	case TypeDataset:
		info.Dataset = analyzeDataset(info.Files)
	case TypeONNX:
		info.ONNX = analyzeONNX(info.Files)
	case TypeTransformers:
		if _, ok := info.Metadata["quantization_config.json"]; ok {
			info.Quantized = analyzeQuantized(info.Metadata)
		}
		// For transformers, first try to detect specialized types from metadata
		specializedType := detectSpecializedType(info.Files, info.Metadata)
		if specializedType != "" {
			info.Type = specializedType
			info.TypeDescription = info.Type.Description()
			// Re-run analysis for the specialized type
			a.analyzeTypeSpecific(info)
			return
		}
		// Standard transformers analysis
		info.Transformers = analyzeTransformers(info.Files, info.Metadata)
	case TypeGeneric:
		// For generic, try to detect specialized types from metadata
		specializedType := detectSpecializedType(info.Files, info.Metadata)
		if specializedType != "" {
			info.Type = specializedType
			info.TypeDescription = info.Type.Description()
			// Re-run analysis for the specialized type
			a.analyzeTypeSpecific(info)
			return
		}
		// Quantization-only repositories (root weights plus a root
		// quantization_config.json, no root architecture config) stay
		// generic; project their quantization info without reclassifying
		// or changing selectable items (issue #123).
		if hasRootWeights(info.Files) {
			if _, ok := info.Metadata["quantization_config.json"]; ok {
				info.Quantized = analyzeQuantized(info.Metadata)
			}
		}
	}

	// For specialized types, run the appropriate analyzer
	switch info.Type {
	case TypeAudio:
		info.Audio = analyzeAudio(info.Files, info.Metadata)
	case TypeVision:
		info.Vision = analyzeVision(info.Files, info.Metadata)
	case TypeMultimodal:
		info.Multimodal = analyzeMultimodal(info.Files, info.Metadata)
	}
}

// humanSize formats bytes as human-readable size.
func humanSize(b int64) string {
	const unit = 1024
	if b < unit {
		return fmt.Sprintf("%d B", b)
	}
	div, exp := int64(unit), 0
	for n := b / unit; n >= unit; n /= unit {
		div *= unit
		exp++
	}
	return fmt.Sprintf("%.1f %ciB", float64(b)/float64(div), "KMGTPE"[exp])
}

// hfModelInfo holds the fields from GET /api/models/{repo} that we need for
// upstream-mmproj discovery and model-chain building.
type hfModelInfo struct {
	ID       string                 `json:"id"`
	Tags     []string               `json:"tags"`
	CardData map[string]interface{} `json:"cardData"`
}

// fetchModelInfo calls GET /api/models/{repo} and returns the relevant fields.
// Non-fatal: callers should treat errors as "no metadata available".
func (a *Analyzer) fetchModelInfo(ctx context.Context, repo string) (*hfModelInfo, error) {
	reqURL := fmt.Sprintf("%s/api/models/%s", a.endpoint, repo)
	req, err := http.NewRequestWithContext(ctx, "GET", reqURL, nil)
	if err != nil {
		return nil, err
	}
	a.addAuth(req)

	resp, err := a.client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	if resp.StatusCode != 200 {
		return nil, fmt.Errorf("fetchModelInfo %s: %s", repo, resp.Status)
	}

	var info hfModelInfo
	if err := json.NewDecoder(resp.Body).Decode(&info); err != nil {
		return nil, fmt.Errorf("decode model info %s: %w", repo, err)
	}
	return &info, nil
}

// baseModelsFrom extracts base_model string(s) from HF card data.
// The field may be a bare string or a JSON array of strings.
func baseModelsFrom(cardData map[string]interface{}) []string {
	if cardData == nil {
		return nil
	}
	v, ok := cardData["base_model"]
	if !ok {
		return nil
	}
	switch bm := v.(type) {
	case string:
		if bm != "" {
			return []string{bm}
		}
	case []interface{}:
		var out []string
		for _, item := range bm {
			if s, ok := item.(string); ok && s != "" {
				out = append(out, s)
			}
		}
		return out
	}
	return nil
}

// relationFromTags extracts the relationship type of a repo to its base model
// from its HF tags (e.g. "base_model:finetune:..." → "finetune").
func relationFromTags(tags []string) string {
	for _, tag := range tags {
		switch {
		case strings.HasPrefix(tag, "base_model:finetune:"):
			return "finetune"
		case strings.HasPrefix(tag, "base_model:quantized:"):
			return "quantized"
		case strings.HasPrefix(tag, "base_model:merge:"):
			return "merge"
		case strings.HasPrefix(tag, "base_model:adapter:"):
			return "adapter"
		}
	}
	return ""
}

// fetchMMProjFromRepo fetches the file tree of repo and returns any mmproj
// GGUF files it contains. Returns nil when the repo doesn't exist or has none.
func (a *Analyzer) fetchMMProjFromRepo(ctx context.Context, repo string) []FileInfo {
	files, _, err := a.fetchFileTree(ctx, repo, false, "main")
	if err != nil {
		return nil
	}
	var out []FileInfo
	for _, f := range files {
		if strings.HasSuffix(strings.ToLower(f.Name), ".gguf") && isMMProjFile(f.Name) {
			out = append(out, f)
		}
	}
	return out
}

// buildModelChain populates GGUFInfo.ModelChain and, when the current repo has
// no local mmproj files, searches upstream repos for them (up to maxDepth
// base_model hops). Errors are non-fatal; partial results are used as-is.
func (a *Analyzer) buildModelChain(ctx context.Context, info *RepoInfo) {
	if info.Type != TypeGGUF || info.GGUF == nil {
		return
	}

	modelInfo, err := a.fetchModelInfo(ctx, info.Repo)
	if err != nil {
		return
	}

	baseModels := baseModelsFrom(modelInfo.CardData)
	if len(baseModels) == 0 {
		return // no lineage declared
	}

	// Build the chain backwards (current repo first, oldest ancestor last),
	// then reverse at the end. This makes the recursion natural.
	currentRelation := relationFromTags(modelInfo.Tags)
	chain := []ModelChainEntry{{Repo: info.Repo, Relation: currentRelation, IsCurrent: true}}

	needMMProj := len(info.GGUF.MMProjFiles) == 0
	const maxDepth = 2

	a.traverseUpstream(ctx, baseModels[0], maxDepth, &chain, needMMProj, info.GGUF)

	// Reverse so the chain reads oldest-ancestor → current.
	for i, j := 0, len(chain)-1; i < j; i, j = i+1, j-1 {
		chain[i], chain[j] = chain[j], chain[i]
	}
	info.GGUF.ModelChain = chain
}

// traverseUpstream walks one hop up the base_model chain. It adds the repo to
// the (reversed) chain slice, tries the "{repo}-GGUF" heuristic for mmproj
// files when needMMProj is true, and recurses if depth > 0.
func (a *Analyzer) traverseUpstream(
	ctx context.Context,
	repo string,
	depth int,
	chain *[]ModelChainEntry,
	needMMProj bool,
	gguf *GGUFInfo,
) {
	if repo == "" {
		return
	}

	// Try the "{owner}/{name}-GGUF" heuristic for the current base repo when
	// it doesn't already look like a GGUF repo. This is where most mmproj
	// files live (e.g. unsloth/Qwen3.5-9B → unsloth/Qwen3.5-9B-GGUF).
	if needMMProj && !strings.HasSuffix(strings.ToLower(repo), "-gguf") {
		candidate := repo + "-GGUF"
		if mmprojs := a.fetchMMProjFromRepo(ctx, candidate); len(mmprojs) > 0 {
			gguf.MMProjFiles = mmprojs
			gguf.MMProjUpstreamRepo = candidate
			needMMProj = false
			// Don't add the -GGUF heuristic repo to the chain; add the
			// safetensors repo that we actually followed the link to.
		}
	}

	// Fetch the model info for this base repo so we can get its relation tag
	// and potentially recurse further.
	ancestorInfo, err := a.fetchModelInfo(ctx, repo)

	relation := ""
	if err == nil {
		relation = relationFromTags(ancestorInfo.Tags)
	}

	*chain = append(*chain, ModelChainEntry{Repo: repo, Relation: relation})

	if depth <= 0 || err != nil {
		return
	}

	nextBases := baseModelsFrom(ancestorInfo.CardData)
	if len(nextBases) == 0 {
		return
	}

	a.traverseUpstream(ctx, nextBases[0], depth-1, chain, needMMProj, gguf)
}

// populateSelectableItems converts type-specific data to unified SelectableItems.
func populateSelectableItems(info *RepoInfo) {
	switch info.Type {
	case TypeGGUF:
		info.SelectableItems = GGUFToSelectableItems(info.GGUF)
	case TypeDiffusers:
		info.SelectableItems = DiffusersToSelectableItems(info.Diffusers)
	case TypeTransformers:
		info.SelectableItems = TransformersToSelectableItems(info.Transformers, info.Files)
	case TypeDataset:
		info.SelectableItems = DatasetToSelectableItems(info.Dataset)
	case TypeLoRA:
		info.RelatedDownloads = LoRAToRelatedDownloads(info.LoRA)
	case TypeGPTQ, TypeAWQ:
		info.SelectableItems = QuantizedToSelectableItems(info.Quantized, info.Files)
	}
}
