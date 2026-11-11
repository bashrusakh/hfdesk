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

// WeightFileModelKey reduces a file's BASE NAME to the key identifying its
// model, for whole-folder "is this exactly one model?" classification. It
// reuses this package's naming rules: recognized GGUF suffixes
// (IsGGUFFileName/trimGGUFSuffix), the "-NNNNN-of-MMMMM" shard suffix
// (splitSuffixPattern), quantization tokens (the quantPattern matcher behind
// GGUFQuantType), vision-encoder companions (IsMMProjFile — a companion first
// reduces to the model key before its marker), and precision markers
// (precisionMarker).
//
// Three outcomes: a non-weight file returns ("", false, false) and carries no
// model identity (config/tokenizer/notes files never multiply keys); a weight
// file reduced to a non-empty base returns it with (key, true, true); a
// weight-shaped name the rules cannot confidently reduce — an empty result
// after stripping (for example a bare "Q4_K_M.gguf" or "fp16.safetensors"), a
// trailing dot, or an mmproj companion without a model part — returns
// ("", true, false) so callers fail closed. An extended quant token the
// matcher leaves unclassified (for example "model-Q4_K_M_EXTRA") stays part of
// the key, so such names compare as distinct keys rather than being merged.
// Distinct keys from several files identify distinct models conservatively:
// names that merely resemble one another stay distinct keys, which can only
// refuse a folder, never wrongly approve one.
func WeightFileModelKey(name string) (key string, weight, ok bool) {
	base := filepath.Base(name)
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
	if IsMMProjFile(base) {
		marker := strings.Index(strings.ToLower(stem), "mmproj")
		if marker < 0 {
			return fail()
		}
		stem = strings.TrimRight(stem[:marker], ".-_")
	}
	stem = splitSuffixPattern.ReplaceAllString(stem, "")
	stem = strings.TrimRight(stem, ".-_")
	if stem == "" {
		return fail()
	}
	// The quant matcher runs on an uppercase view; strings.ToUpper can change
	// byte length for non-ASCII names, so skip the strip there. Under-stripping
	// keeps more of the name in the key, which can only split keys (refusing a
	// folder), never merge two models.
	if upper := strings.ToUpper(stem); len(upper) == len(stem) {
		if loc := quantPattern.FindStringIndex(upper); loc != nil {
			stem = strings.TrimRight(stem[:loc[0]]+stem[loc[1]:], ".-_ ")
			if stem == "" {
				return fail()
			}
		}
	}
	// Precision markers: mirror precisionMarker's own recognition rules and
	// cut the recognized occurrence ("model.fp16", "model_fp16.something").
	if marker := precisionMarker(stem); marker != "" {
		lowered := strings.ToLower(stem)
		cut := -1
		if lowered == marker {
			cut = 0
		} else {
			for _, sep := range []string{"." + marker + ".", "_" + marker + "."} {
				if i := strings.Index(lowered, sep); i >= 0 {
					cut = i
					break
				}
			}
			if cut < 0 {
				for _, suffix := range []string{"." + marker, "_" + marker} {
					if strings.HasSuffix(lowered, suffix) {
						cut = len(lowered) - len(suffix)
						break
					}
				}
			}
		}
		if cut < 0 {
			return fail()
		}
		stem = strings.TrimRight(stem[:cut], ".-_")
		if stem == "" {
			return fail()
		}
	}
	return stem, true, true
}
