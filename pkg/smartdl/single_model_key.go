// Copyright 2025
// SPDX-License-Identifier: Apache-2.0

package smartdl

import (
	"path/filepath"
	"sort"
	"strings"
)

// weightModelKeyExtensions are the non-GGUF weight payload extensions
// WeightFileModelKey recognizes. GGUF suffixes stay owned by IsGGUFFileName;
// descriptive companions such as OpenVINO ".xml" are deliberately absent —
// the payload file beside them carries the identity.
var weightModelKeyExtensions = map[string]bool{
	".safetensors": true,
	".bin":         true,
	".pt":          true,
	".pth":         true,
	".ckpt":        true,
	".onnx":        true,
	".msgpack":     true,
	".tflite":      true,
}

// diffusersComponentDirs is DERIVED from this package's existing pipeline
// component knowledge (requiredComponents) and restates no separate list.
var diffusersComponentDirs = func() map[string]bool {
	dirs := map[string]bool{}
	for _, components := range requiredComponents {
		for _, component := range components {
			dirs[component] = true
		}
	}
	return dirs
}()

// asciiFoldEqualAt reports whether the pure-ASCII needle matches hay at byte
// offset at after folding ASCII letter case byte-by-byte; non-ASCII bytes are
// compared unchanged. The offset — and any slice taken from it — is therefore
// a byte boundary of the ORIGINAL string. A total-length check on a
// case-rewritten shadow string is the wrong check and is deliberately not
// used: Unicode case mapping can grow one rune (Ⱥ, U+023A: 2→3 bytes) and
// shrink another (ẞ, U+1E9E: 3→2 bytes), so compensating runes keep the total
// length equal while shifting every offset after them.
func asciiFoldEqualAt(hay string, at int, needle string) bool {
	if at < 0 || at+len(needle) > len(hay) {
		return false
	}
	for j := 0; j < len(needle); j++ {
		h, n := hay[at+j], needle[j]
		if h >= 'A' && h <= 'Z' {
			h += 'a' - 'A'
		}
		if n >= 'A' && n <= 'Z' {
			n += 'a' - 'A'
		}
		if h != n {
			return false
		}
	}
	return true
}

// asciiFoldIndex returns the byte offset of the first case-insensitive ASCII
// occurrence of needle in hay, or -1. The needle is pure ASCII, so no
// non-ASCII byte of hay can match it and every reported offset is a valid
// byte boundary of the original string.
func asciiFoldIndex(hay, needle string) int {
	for i := 0; i+len(needle) <= len(hay); i++ {
		if asciiFoldEqualAt(hay, i, needle) {
			return i
		}
	}
	return -1
}

// splitModelKeyPath splits a folder-relative path into its parent directory
// components and its base name. Slash is always a separator (fs.WalkDir
// contract); the platform separator covers filepath.Rel results. On a
// platform where the other character may legally occur inside a file name it
// is NOT treated as a separator, so a name can never be split against the
// platform's own rules.
func splitModelKeyPath(relPath string) (dirs []string, base string) {
	segments := strings.FieldsFunc(relPath, func(r rune) bool {
		return r == '/' || r == filepath.Separator
	})
	if len(segments) == 0 {
		return nil, ""
	}
	return segments[:len(segments)-1], segments[len(segments)-1]
}

// reduceWeightFileKey is the per-file core of the classification: it
// classifies the folder-relative path and strips the name down to its
// reduced model base, returning the parent directory components unchanged so
// the caller can scope them. The reduction is strictly right-anchored
// (boundary-safe): a suffix segment is stripped only when it reaches the END
// of the name — the "-<quant>" token at the end, the "-NNNNN-of-MMMMM" shard
// suffix at the end (the $-anchored splitSuffixPattern), a precision marker
// at the end, and only the canonical "<model>.mmproj-<quant>" companion
// shape (the literal ".mmproj-" delimiter, found with the byte-aligned ASCII
// search). Every slice operates on the ORIGINAL byte string: the companion
// and precision cuts match ASCII byte-wise (asciiFoldEqualAt), and the quant
// cut uses Go regexp byte offsets into the input as given ((?i) folds case
// internally, the reported offsets are the original's) — so no case-rewritten
// shadow string can shift a boundary. An interior occurrence never cuts.
func reduceWeightFileKey(relPath string) (dirs []string, reduced string, weight, ok bool) {
	dirs, base := splitModelKeyPath(relPath)
	if base == "" {
		return nil, "", false, false
	}
	lower := strings.ToLower(base)
	var stem string
	switch {
	case IsGGUFFileName(base):
		stem = trimGGUFSuffix(base)
	case weightModelKeyExtensions[filepath.Ext(lower)]:
		stem = base[:len(base)-len(filepath.Ext(base))]
	default:
		return nil, "", false, false
	}
	weight = true
	fail := func() ([]string, string, bool, bool) { return nil, "", true, false }

	// Canonical companion shape only: "<model>.mmproj-<quant>". A name that
	// merely contains "mmproj" without the ".mmproj-" delimiter is NOT cut,
	// and the prefix slice is taken at the delimiter's byte offset in the
	// original stem.
	if idx := asciiFoldIndex(stem, ".mmproj-"); idx >= 0 {
		if idx == 0 {
			return fail() // companion without a model part
		}
		stem = strings.TrimRight(stem[:idx], ".-_")
		if stem == "" {
			return fail()
		}
	}

	// Right-anchored suffix loop: every strip removes only a segment that
	// reaches the end of the stem, the stem strictly shrinks each iteration,
	// so the loop terminates and never cuts an interior span.
	for {
		changed := false
		// Shard suffix: splitSuffixPattern is $-anchored, so only the trailing
		// "-NNNNN-of-MMMMM" segment matches; an interior span never does.
		if next := splitSuffixPattern.ReplaceAllString(stem, ""); next != stem {
			stem = strings.TrimRight(next, ".-_ ")
			changed = true
		}
		// Quantization token: the match must END at the end of the stem (a
		// leftmost match that stops short of the end is declined), and the
		// cut is byte-safe by contract: Go's regexp matches the ORIGINAL
		// string ((?i) performs any case folding internally) and reports byte
		// offsets into that original, so stem[:loc[0]] is an exact byte
		// boundary no matter how case mapping would rewrite lengths.
		for _, loc := range quantPattern.FindAllStringIndex(stem, -1) {
			if loc[1] == len(stem) {
				stem = strings.TrimRight(stem[:loc[0]], "-_. ")
				changed = true
				break
			}
		}
		// Precision marker: only a trailing form (or the whole stem), matched
		// byte-wise on the original; an interior occurrence is declined and
		// stays in the key.
		if marker := precisionMarker(stem); marker != "" {
			cut := len(stem) - len(marker) - 1
			switch {
			case len(stem) == len(marker) && asciiFoldEqualAt(stem, 0, marker):
				stem = ""
				changed = true
			case asciiFoldEqualAt(stem, cut, "."+marker), asciiFoldEqualAt(stem, cut, "_"+marker):
				stem = strings.TrimRight(stem[:cut], "-_.")
				changed = true
			}
		}
		if !changed {
			break
		}
		if stem == "" {
			return fail()
		}
	}
	if stem == "" {
		return fail()
	}
	return dirs, stem, true, true
}

// WeightFileModelKey reduces a file's folder-RELATIVE path to the key
// identifying its model, for whole-folder "is this exactly one model?"
// classification. See reduceWeightFileKey for the strictly right-anchored,
// byte-aligned rule.
//
// Directory scoping: pipeline membership is decided ONLY by the first
// directory component — only a first component in diffusersComponentDirs is
// dropped from this per-file key; any component-NAMED directory deeper in
// the path is an ordinary directory and every directory component is kept
// verbatim. An interior component must never be dropped: `a/unet/x` and
// `a/x` are distinct folder-relative names and stay distinct keys.
//
// The exact merge claim: two distinct file names are never merged by an
// interior excision; the only designed merges are quant/shard/precision
// variants of one name, the canonical "<model>.mmproj-<quant>" companion,
// and — at the folder level, see SingleModelFolderKeys — one pipeline's
// component roles (strictly first-component membership with the sub-path
// below the role directory preserved).
//
// Three outcomes: a non-weight file returns ("", false, false) and carries
// no model identity (config/tokenizer/notes files never multiply keys); a
// weight file reduced to a non-empty key returns it with (key, true, true);
// a weight-shaped name the rules cannot confidently reduce — a key empty
// after stripping (for example a bare "Q4_K_M.gguf" or "fp16.safetensors")
// or a ".mmproj-" companion without a model part — returns ("", true, false)
// so callers fail closed.
func WeightFileModelKey(relPath string) (key string, weight, ok bool) {
	dirs, reduced, isWeight, good := reduceWeightFileKey(relPath)
	if !isWeight || !good {
		return "", isWeight, good
	}
	kept := make([]string, 0, len(dirs))
	for i, dir := range dirs {
		if dir == "" {
			continue
		}
		if i == 0 && diffusersComponentDirs[dir] {
			continue
		}
		kept = append(kept, dir)
	}
	key = strings.Join(kept, "/")
	if key != "" {
		key += "/"
	}
	return key + reduced, true, true
}

// pipelineModelKey is the shared key contributed by uniformly named Diffusers
// component roles. It is prefixed with ":" so it cannot collide with any
// normal reduced name in practice; if a literal collision ever occurs, the
// pair counts as two keys and the folder is refused (the safe direction).
const pipelineModelKey = ":pipeline"

// SingleModelFolderKeys classifies a whole folder's weight files to the set
// of model keys for whole-folder "exactly one model?" eligibility. Non-weight
// files are ignored; any weight-shaped name that cannot be classified makes
// the second result true so callers fail closed.
//
// Pipeline-key rule (owner decision), layered on the per-file reduction:
//
//  1. a weight file whose FIRST directory is a recognized Diffusers
//     component directory is a pipeline member — membership is strictly
//     first-component-based; every uniformly named component role
//     contributes ONE shared pipeline key, so a real tree
//     (text_encoder/pytorch_model.bin + unet/diffusion_pytorch_model +
//     vae/diffusion_pytorch_model) is one model with role-named components;
//  2. within one component directory, the role's recorded base includes the
//     sub-path BELOW the role directory plus the reduced file base
//     (unet/a/pytorch_lora_weights and unet/b/pytorch_lora_weights are two
//     distinct bases) and also distinct reduced bases alone — so two
//     models' parts, or an adapter pile, inside one role directory are
//     refused; a plain component payload records only its reduced base,
//     keeping the uniform-role pipeline merge for the real SD/SDXL shapes;
//  3. a pipeline key together with any non-component weight key gives two or
//     more keys — pipeline weights mixed with top-level weights are refused;
//  4. non-component keys keep EVERY directory component verbatim: an
//     interior component is never dropped, so a/unet/x and a/x — and
//     a/b/unet/x and a/b/x — stay distinct.
//
// Accepted floor, stated honestly: name-based classification cannot tell one
// model's component directory from another's when each role directory is
// internally uniform (a junk folder mixing modelA's unet with modelB's vae
// stays eligible) — component-role parts of one folder are treated as one
// pipeline. That is the conservative floor of name-based classification, not
// a content claim.
//
// Keys are deduplicated per source and returned sorted, so the result is
// deterministic.
func SingleModelFolderKeys(paths []string) ([]string, bool) {
	general := map[string]bool{}
	roleBases := map[string]map[string]bool{}
	unclassified := false
	for _, relPath := range paths {
		dirs, reduced, weight, ok := reduceWeightFileKey(relPath)
		if !weight {
			continue
		}
		if !ok {
			unclassified = true
			continue
		}
		if len(dirs) > 0 && diffusersComponentDirs[dirs[0]] {
			if roleBases[dirs[0]] == nil {
				roleBases[dirs[0]] = map[string]bool{}
			}
			// The role's recorded base includes the sub-path BELOW the role
			// directory (F9): unet/a/lora and unet/b/lora are two distinct
			// bases, so an adapter pile inside a role directory refuses
			// exactly as it does at top level. A plain component payload
			// records only its reduced base, keeping the uniform-role
			// pipeline merge for the real SD/SDXL shapes.
			basis := reduced
			if len(dirs) > 1 {
				basis = strings.Join(dirs[1:], "/") + "/" + reduced
			}
			roleBases[dirs[0]][basis] = true
			continue
		}
		// General keys keep EVERY directory component verbatim (F8): this
		// branch only runs when the FIRST component is not a pipeline
		// component, and a component-NAMED directory deeper in the path must
		// never be dropped — `a/unet/x` and `a/x` are distinct names.
		key := strings.Join(dirs, "/")
		if key != "" {
			key += "/"
		}
		general[key+reduced] = true
	}
	if unclassified {
		return nil, true
	}
	keys := make([]string, 0, len(general)+len(roleBases)+1)
	for key := range general {
		keys = append(keys, key)
	}
	pipeline := false
	for _, bases := range roleBases {
		if len(bases) > 1 {
			for base := range bases {
				keys = append(keys, base)
			}
		} else {
			pipeline = true
		}
	}
	if pipeline {
		// Appended even when a literal ":pipeline" general key exists: the
		// pair then counts as two keys and the folder is refused (the safe
		// direction of a sentinel collision), which a post-append dedup would
		// destroy — so dedup happens per source only.
		keys = append(keys, pipelineModelKey)
	}
	sort.Strings(keys)
	return keys, false
}
