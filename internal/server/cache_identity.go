package server

import (
	"os"
	"path/filepath"

	"github.com/bashrusakh/hfdesk/pkg/hfdownloader"
)

// cacheEnvironment captures only storage ENV/defaults, not editable preferences.
// The pointer is immutable and shared by each complete config snapshot.
type cacheEnvironment struct {
	defaultRoot string
	rootSource  string
	hub         string
	pathBase    string
}

func (cfg Config) captureCacheEnvironment() Config {
	if cfg.cacheEnv == nil {
		pathBase, _ := filepath.Abs(".")
		source := "default"
		if os.Getenv("HF_HOME") != "" {
			source = "HF_HOME"
		}
		root := hfdownloader.DefaultCacheDir()
		if absolute, err := filepath.Abs(root); err == nil {
			root = absolute
		}
		hub := os.Getenv("HF_HUB_CACHE")
		if hub != "" {
			if absolute, err := filepath.Abs(hub); err == nil {
				hub = absolute
			}
		}
		cfg.cacheEnv = &cacheEnvironment{defaultRoot: root, rootSource: source, hub: hub, pathBase: pathBase}
	}
	return cfg
}

func (cfg Config) cacheRoot() string {
	if cfg.CacheDir != "" {
		return cfg.CacheDir
	}
	return cfg.captureCacheEnvironment().cacheEnv.defaultRoot
}

func (cfg Config) hubDirSource() string {
	cfg = cfg.captureCacheEnvironment()
	if cfg.cacheEnv.hub != "" {
		return "HF_HUB_CACHE"
	}
	if cfg.CacheDir != "" {
		return "cacheDir"
	}
	return cfg.cacheEnv.rootSource
}

func (cfg Config) cacheForRoot(root string) *hfdownloader.HFCache {
	cfg = cfg.captureCacheEnvironment()
	if root == "" {
		root = cfg.cacheRoot()
	}
	root = configuredPath(root, cfg.cacheEnv.pathBase)
	hub := cfg.cacheEnv.hub
	if hub == "" {
		hub = filepath.Join(root, "hub")
	}
	return hfdownloader.NewHFCacheResolved(root, hub, 0)
}

func (cfg Config) cache() *hfdownloader.HFCache { return cfg.cacheForRoot(cfg.cacheRoot()) }

// jobCache is also used for manually constructed legacy jobs. Loaded jobs are
// resolved once in LoadState, so normal runners/cleanup never take this fallback.
func jobCache(cfg Config, job *Job) *hfdownloader.HFCache {
	if job.HubDir != "" {
		return hfdownloader.NewHFCacheResolved(job.OutputDir, job.HubDir, 0)
	}
	return cfg.cacheForRoot(job.OutputDir)
}

// A repository directory or partial blob is not usable cached content. Require
// a snapshot file that resolves inside this repository (including zero-byte
// files), so external Python caches work without HFDesk's friendly manifest.
//
// The selected H (or a shared-store ancestor) may itself be a symlink, e.g.
// /mnt/hf-link -> /data/store. Snapshot leaves then resolve under the real
// store while rd.Path() stays lexical, so comparing against the lexical root
// would always fail and hide a genuinely complete repository. Resolve the Hub
// directory (the repository's parent) and keep the repository name lexical:
// this recognizes real content under a symlinked Hub while still refusing a
// repository that is itself a symlink out of the Hub. If resolution fails
// (Hub/repo not created yet), keep the lexical root so the check is
// fail-closed. Detection only; deletion/mirror confinement is unchanged.
func hubRepoHasContent(rd *hfdownloader.RepoDir) bool {
	root := rd.Path()
	if resolvedHub, err := filepath.EvalSymlinks(filepath.Dir(root)); err == nil {
		root = filepath.Join(resolvedHub, filepath.Base(root))
	}
	found := false
	_ = filepath.Walk(rd.SnapshotsDir(), func(path string, info os.FileInfo, err error) error {
		if err != nil || info.IsDir() {
			return nil
		}
		real, err := filepath.EvalSymlinks(path)
		if err != nil || !hfdownloader.PathInside(root, real) {
			return nil
		}
		if st, err := os.Stat(real); err == nil && st.Mode().IsRegular() {
			found = true
		}
		return nil
	})
	return found
}

// A friendly manifest may outlive a restart that selects another H. Its path
// is historical completion evidence only when both existing directories are
// confirmed by the filesystem to be the same object; it never grants mutation
// or deletion authority.
func manifestBelongsToRepo(cache *hfdownloader.HFCache, rd *hfdownloader.RepoDir, m *hfdownloader.DownloadManifest) bool {
	if m.RepoPath == "" || !hubRepoHasContent(rd) {
		return false
	}
	path := filepath.FromSlash(m.RepoPath)
	if !filepath.IsAbs(path) {
		path = filepath.Join(cache.Root, path)
	}
	manifestPath := configuredPath(path, cache.Root)
	manifestInfo, err := os.Stat(manifestPath)
	if err != nil || !manifestInfo.IsDir() {
		return false
	}
	repoInfo, err := os.Stat(rd.Path())
	return err == nil && repoInfo.IsDir() && os.SameFile(manifestInfo, repoInfo)
}
