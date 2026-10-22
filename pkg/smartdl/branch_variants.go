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
	maxEXLBranchSizeLookups      = 16
	maxEXLSafeTensorHeaderProbes = 16
	exlBranchLookupWorkers       = 3
	exlBranchLookupTimeout       = 15 * time.Second
)

var (
	exlBPWBranchPattern = regexp.MustCompile(`(?i)(?:^|[/_-])([0-9]+(?:\.[0-9]+)?)bpw(?:$|[/_-])`)
	exlNMBranchPattern  = regexp.MustCompile(`(?i)(?:^|[/_-])([0-9]+)_([0-9]+)(?:$|[/_-])`)
	exlHeadBitsPattern  = regexp.MustCompile(`(?i)(?:^|[/_-])h([0-9]+(?:\.[0-9]+)?)(?:$|[/_-])`)
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
func (a *Analyzer) exlBranchVariantItems(ctx context.Context, info *RepoInfo) ([]SelectableItem, error) {
	if info == nil || info.IsDataset {
		return nil, nil
	}

	repoMethod := exlMethodFromRepo(info.Repo)
	mainMethod := repoMethod
	if mainMethod == "" {
		quantized := info.Quantized
		if quantized == nil {
			quantized = analyzeQuantized(info.Metadata)
		}
		if quantized != nil {
			mainMethod = normalizeEXLMethod(quantized.Method)
		}
	}

	candidates := make([]exlBranchCandidate, 0, len(info.Refs))
	for _, ref := range info.Refs {
		candidate, ok := parseEXLBranchCandidate(ref, repoMethod != "")
		if ok {
			candidates = append(candidates, candidate)
		}
	}
	if len(candidates) == 0 {
		return nil, nil
	}
	if hasWeights, err := a.hasModelWeightFiles(ctx, info.Repo, info.Files); err != nil {
		return nil, err
	} else if hasWeights {
		return nil, nil
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
	// item, but exhausting the sizing budget or an optional lookup error must
	// not truncate this visible branch set.
	itemsByCandidate := make([]SelectableItem, len(candidates))
	includeCandidate := make([]bool, len(candidates))
	seedMethod := repoMethod
	if seedMethod == "" {
		seedMethod = mainMethod
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
	lookupCtx, cancel := context.WithTimeout(ctx, exlBranchLookupTimeout)
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
				results[index] = a.lookupEXLBranch(lookupCtx, info.Repo, candidates[index], repoMethod, mainMethod)
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
		return nil, err
	}

	items := make([]SelectableItem, 0, len(candidates))
	for i := range candidates {
		if includeCandidate[i] {
			items = append(items, itemsByCandidate[i])
		}
	}
	markRecommendedEXLBranch(items)
	return items, nil
}

func (a *Analyzer) lookupEXLBranch(ctx context.Context, repo string, candidate exlBranchCandidate, repoMethod, mainMethod string) exlBranchResult {
	result := exlBranchResult{}
	if ctx.Err() != nil {
		return result
	}

	branchQuantized, _ := a.fetchEXLBranchQuantized(ctx, repo, candidate.revision)

	method := ""
	if branchQuantized != nil && strings.TrimSpace(branchQuantized.Method) != "" {
		method = normalizeEXLMethod(branchQuantized.Method)
		if method == "" {
			// An explicit non-EXL branch config is stronger than a repo-name
			// heuristic and means this ref is not an EXL bitrate variant.
			result.exclude = true
			return result
		}
	}
	if method == "" {
		method = repoMethod
	}
	if method == "" {
		method = mainMethod
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

func parseEXLBranchCandidate(ref RepoRef, allowNM bool) (exlBranchCandidate, bool) {
	if ref.Type != "branch" || !safeEXLRevision(ref.Name) {
		return exlBranchCandidate{}, false
	}

	candidate := exlBranchCandidate{revision: ref.Name}
	if matches := exlBPWBranchPattern.FindStringSubmatch(ref.Name); len(matches) == 2 {
		candidate.bpw, _ = strconv.ParseFloat(matches[1], 64)
	} else if allowNM {
		matches := exlNMBranchPattern.FindStringSubmatch(ref.Name)
		if len(matches) != 3 {
			return exlBranchCandidate{}, false
		}
		candidate.bpw, _ = strconv.ParseFloat(matches[1]+"."+matches[2], 64)
	} else {
		return exlBranchCandidate{}, false
	}
	if !validEXLWidth(candidate.bpw) {
		return exlBranchCandidate{}, false
	}
	if matches := exlHeadBitsPattern.FindStringSubmatch(ref.Name); len(matches) == 2 {
		candidate.headBits, _ = strconv.ParseFloat(matches[1], 64)
		if !validEXLWidth(candidate.headBits) {
			candidate.headBits = 0
		}
	}
	return candidate, true
}

func safeEXLRevision(revision string) bool {
	// Match the existing revision checks used before an HF revision is placed
	// in an analyze URL or DOM attribute; slash is intentionally permitted.
	if revision == "" || revision == "." || revision == ".." || strings.ContainsAny(revision, "\\\x00") {
		return false
	}
	for _, r := range revision {
		if r < 0x20 || r == 0x7f {
			return false
		}
	}
	return true
}

// hasModelWeightFiles keeps the existing format checks, but does not equate a
// safetensors extension alone with model weights. A positively identified
// recognized calibration tensor family is calibration data; inconclusive
// headers preserve the previous conservative behavior and count as weights.
func (a *Analyzer) hasModelWeightFiles(ctx context.Context, repo string, files []FileInfo) (bool, error) {
	probeCtx, cancel := context.WithTimeout(ctx, exlBranchLookupTimeout)
	defer cancel()
	safetensorsProbes := 0
	for _, file := range files {
		if err := ctx.Err(); err != nil {
			return false, err
		}
		name := strings.ToLower(filepath.Base(file.Path))
		switch filepath.Ext(name) {
		case ".safetensors":
			if safetensorsProbes == maxEXLSafeTensorHeaderProbes || probeCtx.Err() != nil {
				if err := ctx.Err(); err != nil {
					return false, err
				}
				return true, nil
			}
			safetensorsProbes++
			header, err := a.fetchSafetensorsHeader(probeCtx, repo, "main", file)
			if err != nil {
				if ctxErr := ctx.Err(); ctxErr != nil {
					return false, ctxErr
				}
				return true, nil
			}
			calibrationOnly, err := isCalibrationOnlySafetensorsHeader(header)
			if err != nil || !calibrationOnly {
				if ctxErr := ctx.Err(); ctxErr != nil {
					return false, ctxErr
				}
				return true, nil
			}
		case ".gguf", ".gguf-split", ".onnx", ".pt", ".pth", ".ckpt", ".h5", ".pb":
			return true, nil
		case ".bin":
			if !strings.Contains(name, "tokenizer") {
				return true, nil
			}
		}
	}
	if err := ctx.Err(); err != nil {
		return false, err
	}
	return false, nil
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
