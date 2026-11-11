// Copyright 2025
// SPDX-License-Identifier: Apache-2.0

package smartdl

import (
	"path/filepath"
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
// component knowledge (requiredComponents) and restates no separate list: a
// weight file directly under one of these directories belongs to the tree's
// component, so the directory is dropped from the model key (unet/… and
// vae/… of one Diffusers tree share a key). Every other parent directory is
// KEPT in the key, so same-named files in sibling ordinary folders stay
// distinct.
var diffusersComponentDirs = func() map[string]bool {
	dirs := map[string]bool{}
	for _, components := range requiredComponents {
		for _, component := range components {
			dirs[component] = true
		}
	}
	return dirs
}()

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

// WeightFileModelKey reduces a file's folder-RELATIVE path to the key
// identifying its model, for whole-folder "is this exactly one model?"
// classification.
//
// The reduction is strictly right-anchored (boundary-safe): a suffix segment
// is stripped only when it reaches the END of the name — the "-<quant>"
// token at the end, the "-NNNNN-of-MMMMM" shard suffix at the end (the
// $-anchored splitSuffixPattern), a precision marker at the end, and only the
// canonical "<model>.mmproj-<quant>" companion shape (the literal ".mmproj-"
// delimiter). An interior occurrence never cuts: "tiny-Q4_K_M-llama" keeps
// its whole name and "x-mmproj-y" is not a companion, so two distinct file
// names can never be merged into one key by an interior excision — an
// unsafe-direction merge is impossible by construction, and a strip that
// does not reach the boundary is simply declined (a UX loss, never a data
// risk).
//
// Parent directories: a recognized Diffusers component directory (derived
// from requiredComponents) is dropped from the key; any other parent
// directory is kept, so a/pytorch_lora_weights and b/pytorch_lora_weights
// stay two distinct keys while unet/… and vae/… of one tree share one.
//
// Three outcomes: a non-weight file returns ("", false, false) and carries
// no model identity (config/tokenizer/notes files never multiply keys); a
// weight file reduced to a non-empty key returns it with (key, true, true);
// a weight-shaped name the rules cannot confidently reduce — a key empty
// after stripping (for example a bare "Q4_K_M.gguf" or "fp16.safetensors")
// or a ".mmproj-" companion without a model part — returns ("", true, false)
// so callers fail closed. Extended or interior tokens the rules decline to
// strip stay part of the key, so such names compare as distinct keys rather
// than being merged.
func WeightFileModelKey(relPath string) (key string, weight, ok bool) {
	dirs, base := splitModelKeyPath(relPath)
	if base == "" {
		return "", false, false
	}
	lower := strings.ToLower(base)
	var stem string
	switch {
	case IsGGUFFileName(base):
		stem = trimGGUFSuffix(base)
	case weightModelKeyExtensions[filepath.Ext(lower)]:
		stem = base[:len(base)-len(filepath.Ext(base))]
	default:
		return "", false, false
	}
	weight = true
	fail := func() (string, bool, bool) { return "", true, false }

	// Canonical companion shape only: "<model>.mmproj-<quant>". A name that
	// merely contains "mmproj" without the ".mmproj-" delimiter is NOT cut.
	// The cut needs a length-stable lowercase so the ASCII-delimited prefix
	// slice stays exact; otherwise the name is kept whole (declined strip).
	if lowered := strings.ToLower(stem); len(lowered) == len(stem) {
		if idx := strings.Index(lowered, ".mmproj-"); idx >= 0 {
			if idx == 0 {
				return fail() // companion without a model part
			}
			stem = strings.TrimRight(stem[:idx], ".-_")
			if stem == "" {
				return fail()
			}
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
		// Quantization token: the match must END at the end of the stem; a
		// leftmost match that stops short of the end is declined.
		if upper := strings.ToUpper(stem); len(upper) == len(stem) {
			for _, loc := range quantPattern.FindAllStringIndex(upper, -1) {
				if loc[1] == len(upper) {
					stem = strings.TrimRight(stem[:loc[0]], "-_. ")
					changed = true
					break
				}
			}
		}
		// Precision marker: only the trailing form (or the whole stem); an
		// interior ".fp16." occurrence is declined and stays in the key.
		if marker := precisionMarker(stem); marker != "" {
			lowered := strings.ToLower(stem)
			if len(lowered) != len(stem) {
				// length-shifting lowercase: decline the strip
			} else if lowered == marker {
				stem = ""
				changed = true
			} else if strings.HasSuffix(lowered, "."+marker) || strings.HasSuffix(lowered, "_"+marker) {
				stem = strings.TrimRight(stem[:len(stem)-len(marker)-1], "-_.")
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

	kept := make([]string, 0, len(dirs))
	for _, dir := range dirs {
		if dir != "" && !diffusersComponentDirs[dir] {
			kept = append(kept, dir)
		}
	}
	key = strings.Join(kept, "/")
	if key != "" {
		key += "/"
	}
	return key + stem, true, true
}
