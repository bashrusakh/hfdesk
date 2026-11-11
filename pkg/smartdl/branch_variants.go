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
	maxEXLBranchSizeLookups = 16
	exlBranchLookupWorkers  = 3
	exlBranchLookupTimeout  = 15 * time.Second
)

// exlBranchLookupWait is the shared optional-metadata budget for one branch
// discovery pass. It is a variable so a focused test can shorten the wait.
var exlBranchLookupWait = exlBranchLookupTimeout

// An N_M branch name encodes a bitrate as <whole>_<fraction> (4_25 is 4.25 BPW).
// Bounding both parts keeps date-like or arbitrary refs out of the variant list.
const (
	maxEXLNMBranchWholeBits = 64
	maxEXLNMBranchFraction  = 99
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

// exlBranchDisplay is the display-only enrichment state for one branch row.
// Every field is optional presentation data; none of it ever decides which
// rows exist.
type exlBranchDisplay struct {
	method   string
	bpw      float64
	headBits float64
}

// exlBranchVariantItems discovers named EXL2/EXL3 bitrate branches for a
// model repo. Ref names provide the complete option list: every branch whose
// name has the recognized EXL variant shape is offered, whatever the main
// tree's files, configs, or headers contain or fail to say. Optional branch
// config and tree lookups enrich method, precision, and size; their outcome
// or timeout only degrades display and can never add, remove, or truncate a
// row. Row presence never depends on main content; the Recommended badge does
// — see mainTreeCarriesPayload.
func (a *Analyzer) exlBranchVariantItems(ctx context.Context, info *RepoInfo) ([]SelectableItem, error) {
	if info == nil || info.IsDataset {
		return nil, nil
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
		return nil, nil
	}
	sort.Slice(candidates, func(i, j int) bool {
		left, right := strings.ToLower(candidates[i].revision), strings.ToLower(candidates[j].revision)
		if left == right {
			return candidates[i].revision < candidates[j].revision
		}
		return left < right
	})

	// Base display enrichment: a non-empty EXL main method wins; otherwise
	// the name heuristics apply — repository name first, then the branch name
	// itself — unless the main configuration declares a recognized non-EXL
	// method, whose known-negative evidence outranks any name guess. None of
	// this decides visibility: every candidate already has its row.
	displays := make([]exlBranchDisplay, len(candidates))
	for i, candidate := range candidates {
		method := mainMethod
		if method == "" && !mainMethodKnown {
			method = repoMethod
			if method == "" {
				method = exlMethodFromName(candidate.revision)
			}
		}
		displays[i] = exlBranchDisplay{method: method, bpw: candidate.bpw, headBits: candidate.headBits}
	}

	// Bounded, best-effort branch-config enrichment. A slow or failing lookup
	// leaves the base display in place; the shared budget never decides which
	// rows are exposed.
	workers := exlBranchLookupWorkers
	if workers > len(candidates) {
		workers = len(candidates)
	}
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
				displays[index] = a.lookupEXLBranch(lookupCtx, info.Repo, candidates[index], displays[index])
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

	// External cancellation is not an optional enrichment failure: propagate
	// it before the size pass starts any further request.
	if err := ctx.Err(); err != nil {
		return nil, err
	}

	items := make([]SelectableItem, len(candidates))
	for i := range candidates {
		items[i] = newEXLBranchItem(candidates[i], displays[i].method, displays[i].bpw, displays[i].headBits)
	}

	// Size only the first candidates under the count limit. The limit
	// controls enrichment, never which variant rows are exposed.
	sizeIndexes := make([]int, 0, maxEXLBranchSizeLookups)
	for i := range items {
		if len(sizeIndexes) == maxEXLBranchSizeLookups || lookupCtx.Err() != nil {
			break
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
					items[index].Size, items[index].SizeHuman = exlBranchTreeSize(files)
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

	// Recommendation precedence (never availability): the Recommended badge —
	// and with it the branch-variant recommended_download — is assigned only
	// where the name-layer classification is Absent; Present and Unknown both
	// withhold it (rule stated precisely at classifyMainTreeWeightNames). No
	// badge means the type analysis's own output stands (a format's file
	// recommendation, or no recommendation for a type that yields no items —
	// the state base produced when Present or Unknown hid the rows). Only the
	// classic EXL branch layout, configs and docs on main with payloads on
	// bitrate branches, sits in the Absent case: there one branch row carries
	// the badge and names the recommended download. Every candidate still
	// gets its row either way.
	if !mainTreeCarriesPayload(info) {
		markRecommendedEXLBranch(items)
	}
	return items, nil
}

// lookupEXLBranch best-effort enriches one branch row's display fields from
// the branch's own quantization config. It never adds or removes a row: a
// failed, timed-out, or unknown lookup leaves the caller's display as-is. A
// branch config outranks repository/main evidence in both directions — an
// explicit EXL method wins, and a recognized non-EXL method clears the
// name-based guess instead of claiming an EXL flavor the config denies.
func (a *Analyzer) lookupEXLBranch(ctx context.Context, repo string, candidate exlBranchCandidate, display exlBranchDisplay) exlBranchDisplay {
	if ctx.Err() != nil {
		return display
	}

	branchQuantized, _ := a.fetchEXLBranchQuantized(ctx, repo, candidate.revision)
	if branchQuantized == nil {
		return display
	}
	if strings.TrimSpace(branchQuantized.Method) != "" {
		if method := normalizeEXLMethod(branchQuantized.Method); method != "" {
			display.method = method
		} else if recognizedNonEXLMethod(branchQuantized.Method) {
			display.method = ""
		}
		// An unknown method string is not known-negative evidence, so the
		// main/repository/branch-name display above still applies.
	}
	if validEXLWidth(branchQuantized.BitsPerWeight) {
		display.bpw = branchQuantized.BitsPerWeight
	}
	if validEXLWidth(branchQuantized.HeadBits) {
		display.headBits = branchQuantized.HeadBits
	}
	return display
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
	// The same checks that guard an HF revision before it is placed in an
	// analyze URL or DOM attribute: slash is intentionally permitted for
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

func exlMethodFromRepo(repo string) string {
	return exlMethodFromName(filepath.Base(repo))
}

// exlMethodFromName reads an EXL flavor hint out of a repository base name or
// a branch name. It is a display heuristic only and never gates visibility.
func exlMethodFromName(name string) string {
	lower := strings.ToLower(name)
	if strings.Contains(lower, "exl3") {
		return "exl3"
	}
	if strings.Contains(lower, "exl2") {
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

// exlBranchDescription renders the row's display text. The method is
// best-effort enrichment and may be unknown, in which case the description
// carries only the bitrate (and head bits when known).
func exlBranchDescription(method string, bpw, headBits float64) string {
	parts := make([]string, 0, 3)
	if method != "" {
		parts = append(parts, strings.ToUpper(method))
	}
	parts = append(parts, strconv.FormatFloat(bpw, 'f', -1, 64)+" BPW")
	if validEXLWidth(headBits) {
		parts = append(parts, "head "+strconv.FormatFloat(headBits, 'f', -1, 64)+" bits")
	}
	return strings.Join(parts, " · ")
}

// mainTreeCarriesPayload reports whether base's NAME-layer weight-role
// classification of the whole main tree would have been anything other than
// Absent — i.e. whether base would have hidden the branch rows on name
// evidence alone (Present or Unknown both hid them; only clean Absent showed
// rows and the badge). Names only: no content probes, no header reads, and
// never a calibration judgment — file names never prove calibration, so a
// calibration-looking or renamed .safetensors counts as payload exactly like
// model.safetensors (user decision «Имена решают (рекомендация)»: any
// .safetensors name is Present; calibration is undetectable by name and is
// never judged).
//
// It decides recommendation precedence, never availability: branch rows are
// offered whatever it returns. Non-Absent → no branch row is badged and the
// type analysis's own output stands: a known format's file recommendation
// where the type has one, no recommendation where it yields no items —
// exactly what base produced when payload hid the branch rows. Absent → the
// branch set is the repository's actual selection: one row is badged and
// names recommended_download, the classic EXL branch layout PR #152
// established.
func mainTreeCarriesPayload(info *RepoInfo) bool {
	if info == nil {
		return false
	}
	return classifyMainTreeWeightNames(info.Files) != weightNameAbsent
}

// Base's three-way name-layer outcome (git show d7ba4fd:pkg/smartdl/
// branch_variants.go, classifyMainWeights/mainWeightEvidence): Absent showed
// the rows and the badge; Present and Unknown both hid the rows.
const (
	weightNameAbsent  = iota // no weight-shaped name at all
	weightNameUnknown        // weight-ish extension without a weight-role marker
	weightNamePresent        // recognized weight name, or any .safetensors by name
)

// classifyMainTreeWeightNames reproduces the name layer of base's
// classifyMainWeights over the whole tree, probes removed, derived
// mechanically from git show d7ba4fd:pkg/smartdl/branch_variants.go:
//   - .safetensors → Present (user names-decide override; replaces base's
//     probe layer, which calibration content and probe failures used to
//     resolve — names decide instead, and calibration is never judged);
//   - {.gguf, .gguf-split, .onnx, .pt, .pth, .ckpt, .h5, .pb, .bin} —
//     base's weight-ish extension list; a .bin named tokenizer*/vocab* is
//     known non-weight evidence and contributes nothing; otherwise
//     isWeightRoleName decides Present, and a non-match is Unknown (base
//     hid rows there too);
//   - any other extension contributes nothing.
//
// Outcome: Present if any file is Present (short-circuit, like base), else
// Unknown if any weight-ish name was unrecognized, else Absent.
func classifyMainTreeWeightNames(files []FileInfo) int {
	unknown := false
	for _, file := range files {
		name := strings.ToLower(filepath.Base(file.Path))
		ext := filepath.Ext(name)
		if ext == ".safetensors" {
			return weightNamePresent
		}
		switch ext {
		case ".gguf", ".gguf-split", ".onnx", ".pt", ".pth", ".ckpt", ".h5", ".pb", ".bin":
			if ext == ".bin" && (strings.Contains(name, "tokenizer") || strings.Contains(name, "vocab")) {
				continue // base: known non-weight evidence, not an unknown role
			}
			if isWeightRoleName(name) {
				return weightNamePresent
			}
			unknown = true
		}
	}
	if unknown {
		return weightNameUnknown
	}
	return weightNameAbsent
}

// isWeightRoleName is base isRecognizedNonSafetensorsWeightName
// (git show d7ba4fd:pkg/smartdl/branch_variants.go) verbatim: the denylist
// keeps training/optimizer/scheduler/trainer-state/tokenizer/vocab artifacts
// as unknown-role evidence; weight roles come only from the marker lists.
func isWeightRoleName(name string) bool {
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

// markRecommendedEXLBranch marks the branch row closest to 4 BPW. Callers
// gate it on mainTreeCarriesPayload: a main tree that carries recognized
// payload never has a branch row displaced as the recommendation.
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
