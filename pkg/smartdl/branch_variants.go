// Copyright 2026
// SPDX-License-Identifier: Apache-2.0

package smartdl

import (
	"context"
	"encoding/json"
	"errors"
	"math"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

const (
	maxEXLBranchSizeLookups       = 16
	maxEXLSafeTensorHeaderProbes  = 16
	exlBranchLookupWorkers        = 3
	exlBranchLookupTimeout        = 15 * time.Second
	exlBranchDiscoveryWarningCode = "exl_branch_discovery_incomplete"
)

// exlBranchLookupWait is the shared optional-metadata budget for one branch
// discovery pass. It is a variable so a focused test can shorten the wait.
var exlBranchLookupWait = exlBranchLookupTimeout

const weightRoleUnrecognizedCause = "weight_role_unrecognized"

const exlBranchDiscoveryWarningMessage = "EXL branch discovery could not be completed because one or more main file roles could not be safely established. Retry analysis to try again."

// An N_M branch name encodes a bitrate as <whole>_<fraction> (4_25 is 4.25 BPW).
// Bounding both parts keeps date-like or arbitrary refs out of the variant list.
const (
	maxEXLNMBranchWholeBits = 64
	maxEXLNMBranchFraction  = 99
)

type mainWeightEvidence uint8

const (
	mainWeightEvidenceAbsent mainWeightEvidence = iota
	mainWeightEvidencePresent
	mainWeightEvidenceUnknown
)

var (
	exlBPWBranchPattern = regexp.MustCompile(`(?i)(?:^|[/_-])([0-9]+(?:\.[0-9]+)?)bpw(?:$|[/_-])`)
	// N_M is a bitrate shape only as the whole final path component, optionally
	// under a nested branch path (exl/4_25). A bare _N_M segment inside an
	// unrelated name (fix_4_2, SC_6_00) is not a variant.
	exlNMBranchPattern = regexp.MustCompile(`(?i)(?:^|/)([0-9]+)_([0-9]+)$`)
	exlHeadBitsPattern = regexp.MustCompile(`(?i)(?:^|[/_-])h([0-9]+(?:\.[0-9]+)?)(?:$|[/_-])`)
)

type exlBranchCandidate struct {
	revision string
	bpw      float64
	headBits float64
}

type exlBranchResult struct {
	item    SelectableItem
	include bool
	exclude bool
}

// exlBranchVariantItems discovers named EXL2/EXL3 bitrate branches for a
// model repo whose main tree has no recognized model weights. Ref
// names provide the complete option list; optional branch config/tree lookups
// enrich method, precision, and size without making those lookups fatal.
func (a *Analyzer) exlBranchVariantItems(ctx context.Context, info *RepoInfo) ([]SelectableItem, *AnalysisWarning, error) {
	if info == nil || info.IsDataset {
		return nil, nil, nil
	}

	repoMethod := exlMethodFromRepo(info.Repo)
	mainMethod := ""
	mainMethodKnown := false
	quantized := info.Quantized
	if quantized == nil {
		quantized = analyzeQuantized(info.Metadata)
	}
	if quantized != nil {
		mainMethod = normalizeEXLMethod(quantized.Method)
		mainMethodKnown = recognizedNonEXLMethod(quantized.Method)
	}

	candidates := make([]exlBranchCandidate, 0, len(info.Refs))
	for _, ref := range info.Refs {
		// An N_M name is a candidate only in its bounded bitrate shape. Its
		// method can still be ambiguous until branch metadata is read, so do
		// not discard an eligible shape merely because neither main nor the
		// repository name establishes the method.
		candidate, ok := parseEXLBranchCandidate(ref)
		if ok {
			candidates = append(candidates, candidate)
		}
	}
	if len(candidates) == 0 {
		return nil, nil, nil
	}
	weightEvidence, causes, err := a.classifyMainWeights(ctx, info.Repo, info.Files)
	if err != nil {
		return nil, nil, err
	}
	switch weightEvidence {
	case mainWeightEvidencePresent:
		return nil, nil, nil
	case mainWeightEvidenceUnknown:
		return nil, &AnalysisWarning{
			Code:      exlBranchDiscoveryWarningCode,
			Message:   exlBranchDiscoveryWarningMessage,
			Causes:    causes,
			Retryable: true,
		}, nil
	}
	sort.Slice(candidates, func(i, j int) bool {
		left, right := strings.ToLower(candidates[i].revision), strings.ToLower(candidates[j].revision)
		if left == right {
			return candidates[i].revision < candidates[j].revision
		}
		return left < right
	})

	// Seed every candidate whose method is already established from the
	// repository/main metadata. Optional lookups may enrich or disqualify an
	// item. A seeded candidate keeps its seeded row (with a zero size) when
	// the sizing budget is exhausted or an optional lookup fails, so those
	// failures never truncate the seeded part of the visible branch set. A
	// candidate whose method can only be established from its own branch
	// metadata has no seed: it is exposed only when the bounded lookup
	// visited it and the branch metadata established the method, so a
	// candidate the lookup never visited is dropped and the visible branch
	// set is truncated by the budget. A non-empty main method wins even
	// when it is EXL evidence (mainMethodKnown is only true for a recognized
	// non-EXL negative); the repository name is only a fallback when the main
	// method is empty and not a known non-EXL negative.
	itemsByCandidate := make([]SelectableItem, len(candidates))
	includeCandidate := make([]bool, len(candidates))
	seedMethod := mainMethod
	if seedMethod == "" && !mainMethodKnown {
		seedMethod = repoMethod
	}
	if seedMethod != "" {
		for i, candidate := range candidates {
			itemsByCandidate[i] = newEXLBranchItem(candidate, seedMethod, candidate.bpw, candidate.headBits)
			includeCandidate[i] = true
		}
	}

	workers := exlBranchLookupWorkers
	if workers > len(candidates) {
		workers = len(candidates)
	}
	results := make([]exlBranchResult, len(candidates))
	jobs := make(chan int)
	lookupCtx, cancel := context.WithTimeout(ctx, exlBranchLookupWait)
	defer cancel()

	var wg sync.WaitGroup
	wg.Add(workers)
	for worker := 0; worker < workers; worker++ {
		go func() {
			defer wg.Done()
			for index := range jobs {
				if lookupCtx.Err() != nil {
					continue
				}
				results[index] = a.lookupEXLBranch(lookupCtx, info.Repo, candidates[index], repoMethod, mainMethod, mainMethodKnown)
			}
		}()
	}

sendJobs:
	for index := range candidates {
		select {
		case jobs <- index:
		case <-lookupCtx.Done():
			break sendJobs
		}
	}
	close(jobs)
	wg.Wait()
	for i, result := range results {
		if result.exclude {
			includeCandidate[i] = false
		} else if result.include {
			itemsByCandidate[i] = result.item
			includeCandidate[i] = true
		}
	}

	// Size only candidates whose EXL method is established. The count limit
	// controls enrichment, never which variant rows are exposed.
	sizeIndexes := make([]int, 0, maxEXLBranchSizeLookups)
	for i, include := range includeCandidate {
		if !include || len(sizeIndexes) == maxEXLBranchSizeLookups || lookupCtx.Err() != nil {
			continue
		}
		sizeIndexes = append(sizeIndexes, i)
	}
	sizeWorkers := exlBranchLookupWorkers
	if sizeWorkers > len(sizeIndexes) {
		sizeWorkers = len(sizeIndexes)
	}
	if sizeWorkers > 0 {
		sizeJobs := make(chan int)
		wg.Add(sizeWorkers)
		for worker := 0; worker < sizeWorkers; worker++ {
			go func() {
				defer wg.Done()
				for index := range sizeJobs {
					if lookupCtx.Err() != nil {
						continue
					}
					files, _, err := a.fetchFileTree(lookupCtx, info.Repo, false, candidates[index].revision)
					if err != nil {
						continue
					}
					itemsByCandidate[index].Size, itemsByCandidate[index].SizeHuman = exlBranchTreeSize(files)
				}
			}()
		}
		for _, index := range sizeIndexes {
			select {
			case sizeJobs <- index:
			case <-lookupCtx.Done():
				break
			}
			if lookupCtx.Err() != nil {
				break
			}
		}
		close(sizeJobs)
		wg.Wait()
	}

	// An external cancellation is not an optional lookup failure. A timeout
	// owned by this optional enrichment pass, however, leaves the main analysis
	// and all completed branch options usable.
	if err := ctx.Err(); err != nil {
		return nil, nil, err
	}

	items := make([]SelectableItem, 0, len(candidates))
	for i := range candidates {
		if includeCandidate[i] {
			items = append(items, itemsByCandidate[i])
		}
	}
	markRecommendedEXLBranch(items)
	return items, nil, nil
}

func (a *Analyzer) lookupEXLBranch(ctx context.Context, repo string, candidate exlBranchCandidate, repoMethod, mainMethod string, mainMethodKnown bool) exlBranchResult {
	result := exlBranchResult{}
	if ctx.Err() != nil {
		return result
	}

	branchQuantized, _ := a.fetchEXLBranchQuantized(ctx, repo, candidate.revision)

	method := ""
	if branchQuantized != nil && strings.TrimSpace(branchQuantized.Method) != "" {
		method = normalizeEXLMethod(branchQuantized.Method)
		if method == "" && recognizedNonEXLMethod(branchQuantized.Method) {
			// A recognized non-EXL quantization config is stronger than a
			// repo-name heuristic and means this ref is not an EXL bitrate
			// variant. An unknown method string is not known-negative
			// evidence, so the main/name fallback below still applies.
			result.exclude = true
			return result
		}
	}
	if method == "" {
		method = mainMethod
	}
	if method == "" && mainMethodKnown {
		result.exclude = true
		return result
	}
	if method == "" {
		method = repoMethod
	}
	if method == "" {
		return result
	}

	bpw := candidate.bpw
	if branchQuantized != nil && validEXLWidth(branchQuantized.BitsPerWeight) {
		bpw = branchQuantized.BitsPerWeight
	}
	headBits := candidate.headBits
	if branchQuantized != nil && validEXLWidth(branchQuantized.HeadBits) {
		headBits = branchQuantized.HeadBits
	}

	item := newEXLBranchItem(candidate, method, bpw, headBits)
	result.item = item
	result.include = true
	return result
}

func exlBranchTreeSize(files []FileInfo) (int64, string) {
	var size int64
	for _, file := range files {
		if file.Size <= 0 {
			continue
		}
		if size > math.MaxInt64-file.Size {
			size = math.MaxInt64
			break
		}
		size += file.Size
	}
	return size, humanSize(size)
}

func newEXLBranchItem(candidate exlBranchCandidate, method string, bpw, headBits float64) SelectableItem {
	return SelectableItem{
		ID:            "exl-branch:" + candidate.revision,
		Label:         candidate.revision,
		Description:   exlBranchDescription(method, bpw, headBits),
		Category:      "quantization",
		Revision:      candidate.revision,
		BranchVariant: true,
		Method:        method,
		BitsPerWeight: bpw,
		HeadBits:      headBits,
	}
}

func (a *Analyzer) fetchEXLBranchQuantized(ctx context.Context, repo, revision string) (*QuantizedInfo, error) {
	var best *QuantizedInfo
	for _, path := range []string{"quantize_config.json", "quantization_config.json", "config.json"} {
		content, err := a.fetchFile(ctx, repo, false, revision, path, false)
		if err != nil {
			if ctx.Err() != nil {
				return best, ctx.Err()
			}
			if !errors.Is(err, errMetadataTooLarge) {
				continue
			}
		}

		var raw interface{}
		if errors.Is(err, errMetadataTooLarge) && path != "config.json" {
			raw, err = decodeQuantizationHead(content)
		} else {
			err = json.Unmarshal(content, &raw)
		}
		if err != nil {
			continue
		}
		if _, ok := raw.(map[string]interface{}); !ok {
			continue
		}
		quantized := analyzeQuantized(map[string]interface{}{path: raw})
		if quantized == nil {
			continue
		}
		if best == nil || (best.Method == "" && quantized.Method != "") {
			best = quantized
		}
		if quantized.Method != "" {
			return quantized, nil
		}
	}
	return best, nil
}

// BranchVariantRevisionShape reports whether revision has the bounded
// EXL2/EXL3 bitrate shape analyzed for branch variants: a bpw token
// (for example 4.00bpw or SC_6.00bpw_H6_V6), or a bitrate-shaped N_M final
// component (for example 4_25 or exl/4_25), with an optional head-bits token.
// It validates only the revision name shape. It deliberately does not prove
// that the ref is a branch rather than a tag; callers needing that distinction
// must consult authoritative ref metadata.
func BranchVariantRevisionShape(revision string) bool {
	_, ok := parseEXLBranchRevisionShape(revision)
	return ok
}

func parseEXLBranchCandidate(ref RepoRef) (exlBranchCandidate, bool) {
	if ref.Type != "branch" {
		return exlBranchCandidate{}, false
	}
	return parseEXLBranchRevisionShape(ref.Name)
}

// parseEXLBranchRevisionShape is the shared revision-name parser used by
// analyzer branch candidates and the API shape validation.
func parseEXLBranchRevisionShape(name string) (exlBranchCandidate, bool) {
	if !safeEXLRevision(name) {
		return exlBranchCandidate{}, false
	}

	candidate := exlBranchCandidate{revision: name}
	if matches := exlBPWBranchPattern.FindStringSubmatch(name); len(matches) == 2 {
		candidate.bpw, _ = strconv.ParseFloat(matches[1], 64)
	} else {
		matches := exlNMBranchPattern.FindStringSubmatch(name)
		if len(matches) != 3 {
			return exlBranchCandidate{}, false
		}
		wholeBits, wholeErr := strconv.Atoi(matches[1])
		fractionBits, fractionErr := strconv.Atoi(matches[2])
		if wholeErr != nil || fractionErr != nil ||
			wholeBits > maxEXLNMBranchWholeBits || fractionBits > maxEXLNMBranchFraction {
			return exlBranchCandidate{}, false
		}
		candidate.bpw, _ = strconv.ParseFloat(matches[1]+"."+matches[2], 64)
	}
	if !validEXLWidth(candidate.bpw) {
		return exlBranchCandidate{}, false
	}
	if matches := exlHeadBitsPattern.FindStringSubmatch(name); len(matches) == 2 {
		candidate.headBits, _ = strconv.ParseFloat(matches[1], 64)
		if !validEXLWidth(candidate.headBits) {
			candidate.headBits = 0
		}
	}
	return candidate, true
}

func safeEXLRevision(revision string) bool {
	// Match the existing revision checks used before an HF revision is placed
	// in an analyze URL or DOM attribute; slash is intentionally permitted for
	// nested branch paths, but no path segment may be "." or "..".
	if revision == "" || strings.ContainsAny(revision, "\\\x00") {
		return false
	}
	for _, segment := range strings.Split(revision, "/") {
		if segment == "." || segment == ".." {
			return false
		}
	}
	for _, r := range revision {
		if r < 0x20 || r == 0x7f {
			return false
		}
	}
	return true
}

// classifyMainWeights preserves the existing positive weight evidence while
// separating an incomplete safetensors probe from a known weight. Non-safetensors
// weight formats and successfully read safetensors headers that are not the
// recognized calibration-only form retain their existing weight classification.
// Probe and classifier failures are uncertainty, not evidence that weights exist.
func (a *Analyzer) classifyMainWeights(ctx context.Context, repo string, files []FileInfo) (mainWeightEvidence, []string, error) {
	unknownCauses := make(map[string]struct{})
	// A recognized weight file elsewhere in the tree is sufficient to preserve
	// ordinary main behavior, regardless of the ordering of safetensors files.
	// Non-safetensors names are evidence only when they name a model-weight
	// role; a training artifact such as training_args.bin or optimizer.pt is
	// not proof that the main revision carries model weights.
	for _, file := range files {
		if err := ctx.Err(); err != nil {
			return mainWeightEvidenceAbsent, nil, err
		}
		name := strings.ToLower(filepath.Base(file.Path))
		switch filepath.Ext(name) {
		case ".gguf", ".gguf-split", ".onnx", ".pt", ".pth", ".ckpt", ".h5", ".pb", ".bin":
			if filepath.Ext(name) == ".bin" && (strings.Contains(name, "tokenizer") || strings.Contains(name, "vocab")) {
				// A tokenizer byte file is known non-weight evidence, not an
				// unknown model role.
				continue
			}
			if isRecognizedNonSafetensorsWeightName(name) {
				return mainWeightEvidencePresent, nil, nil
			}
			unknownCauses[weightRoleUnrecognizedCause] = struct{}{}
		}
	}

	probeCtx, cancel := context.WithTimeout(ctx, exlBranchLookupTimeout)
	defer cancel()
	safetensorsProbes := 0
	for _, file := range files {
		if err := ctx.Err(); err != nil {
			return mainWeightEvidenceAbsent, nil, err
		}
		name := strings.ToLower(filepath.Base(file.Path))
		if filepath.Ext(name) != ".safetensors" {
			continue
		}
		if safetensorsProbes == maxEXLSafeTensorHeaderProbes {
			unknownCauses["probe_limit"] = struct{}{}
			break
		}
		if probeErr := probeCtx.Err(); probeErr != nil {
			if err := ctx.Err(); err != nil {
				return mainWeightEvidenceAbsent, nil, err
			}
			unknownCauses["probe_timeout"] = struct{}{}
			break
		}
		safetensorsProbes++
		header, err := a.fetchSafetensorsHeader(probeCtx, repo, "main", file)
		if err != nil {
			if ctxErr := ctx.Err(); ctxErr != nil {
				return mainWeightEvidenceAbsent, nil, ctxErr
			}
			if errors.Is(err, context.DeadlineExceeded) {
				unknownCauses["probe_timeout"] = struct{}{}
				if probeCtx.Err() != nil {
					break
				}
			} else {
				unknownCauses["header_unavailable"] = struct{}{}
			}
			continue
		}
		calibrationOnly, err := isCalibrationOnlySafetensorsHeader(header)
		if err != nil {
			if ctxErr := ctx.Err(); ctxErr != nil {
				return mainWeightEvidenceAbsent, nil, ctxErr
			}
			unknownCauses["header_uninterpretable"] = struct{}{}
			continue
		}
		if !calibrationOnly {
			return mainWeightEvidencePresent, nil, nil
		}
	}
	if err := ctx.Err(); err != nil {
		return mainWeightEvidenceAbsent, nil, err
	}
	if len(unknownCauses) == 0 {
		return mainWeightEvidenceAbsent, nil, nil
	}
	causes := make([]string, 0, len(unknownCauses))
	for cause := range unknownCauses {
		causes = append(causes, cause)
	}
	sort.Strings(causes)
	return mainWeightEvidenceUnknown, causes, nil
}

// isRecognizedNonSafetensorsWeightName reports whether a non-safetensors file
// name identifies a model-weight role. It deliberately rejects training,
// optimizer, scheduler, trainer-state, and tokenizer artifacts so those files
// remain unknown role evidence rather than confirmed model weights.
func isRecognizedNonSafetensorsWeightName(name string) bool {
	lower := strings.ToLower(filepath.Base(name))
	for _, marker := range []string{"training", "optimizer", "scheduler", "trainer_state", "tokenizer", "vocab"} {
		if strings.Contains(lower, marker) {
			return false
		}
	}
	switch filepath.Ext(lower) {
	case ".bin", ".pt", ".pth", ".ckpt", ".h5", ".pb", ".onnx":
		for _, marker := range []string{"model", "weights", "consolidated"} {
			if strings.Contains(lower, marker) {
				return true
			}
		}
		return false
	case ".gguf", ".gguf-split":
		for _, marker := range []string{"model", "weights", "ggml", "consolidated", "q4", "q5", "q6", "q8", "f16", "bf16", "fp16"} {
			if strings.Contains(lower, marker) {
				return true
			}
		}
		return false
	}
	return false
}

func exlMethodFromRepo(repo string) string {
	name := strings.ToLower(filepath.Base(repo))
	if strings.Contains(name, "exl3") {
		return "exl3"
	}
	if strings.Contains(name, "exl2") {
		return "exl2"
	}
	return ""
}

func normalizeEXLMethod(method string) string {
	switch strings.ToLower(strings.TrimSpace(method)) {
	case "exl2":
		return "exl2"
	case "exl3":
		return "exl3"
	default:
		return ""
	}
}

func validEXLWidth(value float64) bool {
	return value > 0 && !math.IsNaN(value) && !math.IsInf(value, 0)
}

func exlBranchDescription(method string, bpw, headBits float64) string {
	parts := []string{strings.ToUpper(method), strconv.FormatFloat(bpw, 'f', -1, 64) + " BPW"}
	if validEXLWidth(headBits) {
		parts = append(parts, "head "+strconv.FormatFloat(headBits, 'f', -1, 64)+" bits")
	}
	return strings.Join(parts, " · ")
}

func markRecommendedEXLBranch(items []SelectableItem) {
	if len(items) == 0 {
		return
	}
	best := 0
	for i := 1; i < len(items); i++ {
		leftDistance := math.Abs(items[i].BitsPerWeight - 4)
		bestDistance := math.Abs(items[best].BitsPerWeight - 4)
		if leftDistance < bestDistance || (leftDistance == bestDistance && items[i].Revision < items[best].Revision) {
			best = i
		}
	}
	items[best].Recommended = true
}
