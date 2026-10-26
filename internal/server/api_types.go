// Copyright 2025
// SPDX-License-Identifier: Apache-2.0

package server

// DownloadRequest is the request body for starting a download.
type DownloadRequest struct {
	Repo               string   `json:"repo"`
	Revision           string   `json:"revision,omitempty"`
	Dataset            bool     `json:"dataset,omitempty"`
	Filters            []string `json:"filters,omitempty"`
	Excludes           []string `json:"excludes,omitempty"`
	AppendFilterSubdir bool     `json:"appendFilterSubdir,omitempty"`
	DryRun             bool     `json:"dryRun,omitempty"`
	// ExactMatch uses case-insensitive matching against repository-relative
	// paths instead of substring matching. Exact filters can select directory
	// components and their descendants with a trailing slash (for example,
	// "weights/"), filename segments (for example, "q6_k" without matching
	// "Q6_K_XL"), an extension (for example, ".safetensors"), or a full
	// slash-containing relative path (for example, "weights/model.gguf"). The
	// explicit "./name" form selects only a literal root-level basename, without
	// changing legacy unqualified basename/token matching. Exact segment
	// boundaries preserve quantization tokens such as "Q6_K".
	ExactMatch bool `json:"exactMatch,omitempty"`
	// LocalDir overrides the download destination for this specific request,
	// writing real files (flat mode) instead of the HF cache layout.
	// When empty, the server-global LocalDir (if set) or HF cache is used.
	LocalDir string `json:"localDir,omitempty"`
	// LocalRepo overrides the folder name used for storing downloaded files.
	// When set, files fetched from Repo are stored as if they belong to LocalRepo.
	// Used when downloading upstream mmproj files alongside the current model's quants.
	LocalRepo string `json:"localRepo,omitempty"`
	// BranchVariant selects a whole EXL2/EXL3 analysis branch named by Revision.
	// It must be an unfiltered model download and is stored isolated from other
	// variants of the same repository.
	BranchVariant bool `json:"branchVariant,omitempty"`
	// RouteKey selects a configured download route (a key from the closed,
	// server-defined set, never a path). When set and configured, the resolved
	// destination is used as the job's flat/local directory. An explicit
	// localDir takes priority; an unknown key is rejected with 400.
	RouteKey string `json:"routeKey,omitempty"`
}

// PlanResponse is the response for a dry-run/plan request.
type PlanResponse struct {
	Repo       string     `json:"repo"`
	Revision   string     `json:"revision"`
	Files      []PlanFile `json:"files"`
	TotalSize  int64      `json:"totalSize"`
	TotalFiles int        `json:"totalFiles"`
}

// PlanFile represents a file in the plan.
type PlanFile struct {
	Path string `json:"path"`
	Size int64  `json:"size"`
	LFS  bool   `json:"lfs"`
}

// SettingsResponse represents current settings.
type SettingsResponse struct {
	Token              string `json:"token,omitempty"`
	CacheDir           string `json:"cacheDir"`
	ConfiguredCacheDir string `json:"configuredCacheDir"`
	EffectiveHubDir    string `json:"effectiveHubDir"`
	HubDirSource       string `json:"hubDirSource"`
	Concurrency        int    `json:"connections"`
	MaxActive          int    `json:"maxActive"`
	MultipartThreshold string `json:"multipartThreshold"`
	MaxSpeed           string `json:"maxSpeed"`
	Verify             string `json:"verify"`
	Retries            int    `json:"retries"`
	StallTimeout       string `json:"stallTimeout"`
	Endpoint           string `json:"endpoint,omitempty"`
	// StorageMode is "local" when the server writes real files into LocalDir,
	// or "cache" when it uses the HF cache layout. Set at startup, read-only.
	StorageMode   string   `json:"storageMode"`
	LocalDir      string   `json:"localDir,omitempty"`
	LocalScanDirs []string `json:"localScanDirs,omitempty"`
	// DownloadRoutes maps internal route keys to destination directories.
	// Opt-in; empty/omitted means routed downloads are disabled.
	DownloadRoutes map[string]string `json:"downloadRoutes,omitempty"`
	// Proxy settings
	Proxy *ProxySettingsResponse `json:"proxy,omitempty"`
	// Config file paths
	ConfigFile  string `json:"configFile,omitempty"`
	TargetsFile string `json:"targetsFile,omitempty"`
}

// ProxySettingsResponse represents proxy configuration in API responses.
type ProxySettingsResponse struct {
	URL                string `json:"url,omitempty"`
	Username           string `json:"username,omitempty"`
	NoProxy            string `json:"noProxy,omitempty"`
	NoEnvProxy         bool   `json:"noEnvProxy,omitempty"`
	InsecureSkipVerify bool   `json:"insecureSkipVerify,omitempty"`
	// Note: Password is intentionally omitted from response for security.
}

// ErrorResponse represents an API error.
type ErrorResponse struct {
	OK          bool            `json:"ok"`
	Error       string          `json:"error"` // Legacy flat message for existing UI/tests.
	Details     string          `json:"details,omitempty"`
	ErrorDetail *APIErrorDetail `json:"error_detail,omitempty"`

	// Gated-access fields are set only for analyze errors caused by a gated
	// repository. HasToken is a pointer so an explicit false is serialized for
	// gated errors while all other error responses omit the field.
	Gated     bool   `json:"gated,omitempty"`
	Repo      string `json:"repo,omitempty"`
	RepoType  string `json:"repo_type,omitempty"`
	AcceptURL string `json:"accept_url,omitempty"`
	HasToken  *bool  `json:"has_token,omitempty"`
}

// APIErrorDetail is the structured error payload for API clients.
type APIErrorDetail struct {
	Code    string `json:"code"`
	Message string `json:"message"`
	Details string `json:"details,omitempty"`
}

// SuccessResponse represents a simple success message.
type SuccessResponse struct {
	Success bool   `json:"success"`
	Message string `json:"message,omitempty"`
}
