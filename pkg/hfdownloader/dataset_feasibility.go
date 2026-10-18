// Copyright 2025
// SPDX-License-Identifier: Apache-2.0

package hfdownloader

import (
	"fmt"
	"path"
	"sort"
	"strings"

	"github.com/bashrusakh/hfdesk/internal/datasetselection"
)

// datasetDecisionPositive is one optional disjunction of direct matches. The
// solver deliberately supports one such clause: direct inventory seeds are
// the only positive conditions needed by the dataset selection decision.
type datasetDecisionPositive struct {
	Input       int
	Alternative datasetDecisionRoleLabel
}

// datasetDecisionPins restricts inputs to actual compiled alternatives.
type datasetDecisionPins map[int]datasetDecisionRoleLabel

// feasibleDatasetDecisionWorld finds one coherent concrete role/token world
// satisfying pins, pre-dedup ineligibility, zero-direct constraints and (when
// present) one positive direct clause. Ineligible is deliberately distinct
// from skipped output: exact/case-fold deduplication happens after eligibility.
func feasibleDatasetDecisionWorld(c datasetDecisionContext, coreAssignment map[uint]bool, pins datasetDecisionPins, ineligible, zeroDirect []int, positive *datasetDecisionPositive) (datasetDecisionWorld, bool) {
	collisions := collisionTokenBits(c)
	units := map[uint]bool{}
	for bit, value := range coreAssignment {
		if !collisions[bit] {
			return datasetDecisionWorld{}, false
		}
		if _, core := isDatasetCoreBit(c, bit); !core {
			return datasetDecisionWorld{}, false
		}
		units[bit] = value
	}
	for bit := range collisions {
		if _, core := isDatasetCoreBit(c, bit); core {
			if _, ok := coreAssignment[bit]; !ok {
				return datasetDecisionWorld{}, false
			}
		}
	}
	for i := range pins {
		if i < 0 || i >= len(c.Inputs) {
			return datasetDecisionWorld{}, false
		}
	}
	roles := make([][]datasetDecisionRoleLabel, len(c.Inputs))
	for i, in := range c.Inputs {
		roles[i] = append([]datasetDecisionRoleLabel(nil), in.Alternatives...)
		if pin, ok := pins[i]; ok {
			if !hasDecisionAlternative(roles[i], pin) {
				return datasetDecisionWorld{}, false
			}
			roles[i] = []datasetDecisionRoleLabel{pin}
		}
	}
	for _, i := range ineligible {
		if datasetDecisionContextErr(c) != nil {
			return datasetDecisionWorld{}, false
		}
		if i < 0 || i >= len(c.Inputs) {
			return datasetDecisionWorld{}, false
		}
		in := c.Inputs[i]
		// Excluded inputs are ineligible regardless of role. Exact duplicates
		// are not exempt: this is a pre-dedup eligibility constraint.
		if in.Excluded {
			continue
		}
		if in.LFS {
			chosen, next, ok := chooseZeroDecisionAlternative(c, i, roles[i], units, collisions)
			if !ok {
				return datasetDecisionWorld{}, false
			}
			roles[i], units = []datasetDecisionRoleLabel{chosen}, next
			continue
		}
		if in.Path == "" || path.Dir(strings.ReplaceAll(in.Path, "\\", "/")) == "." {
			chosen, next, ok := chooseZeroDecisionAlternative(c, i, roles[i], units, collisions)
			if !ok {
				return datasetDecisionWorld{}, false
			}
			roles[i], units = []datasetDecisionRoleLabel{chosen}, next
			continue
		}
		// A nested data alternative with no direct match is ineligible regardless
		// of companion-directory scope, so prefer that explanation first.
		dataAlternatives := make([]datasetDecisionRoleLabel, 0, len(roles[i]))
		foundIneligible := false
		for _, alt := range roles[i] {
			if alt.Role == datasetselection.EvidenceRoleData {
				dataAlternatives = append(dataAlternatives, alt)
			}
		}
		if chosen, next, ok := chooseZeroDecisionAlternative(c, i, dataAlternatives, units, collisions); ok {
			roles[i], units = []datasetDecisionRoleLabel{chosen}, next
			continue
		}
		// Nested files may be retained only as companions, and only when every
		// possible direct seed for an ancestor is absent.
		for _, alt := range roles[i] {
			if alt.Role != datasetselection.EvidenceRoleCompanion {
				continue
			}
			trial := cloneDecisionUnits(units)
			trialRoles := make([][]datasetDecisionRoleLabel, len(roles))
			for j := range roles {
				trialRoles[j] = append([]datasetDecisionRoleLabel(nil), roles[j]...)
			}
			ok := constrainDecisionDirect(c, i, []datasetDecisionRoleLabel{alt}, trial, collisions, false)
			for _, seed := range validDatasetDirectorySeeds(c, in.Directory) {
				if seed >= 0 && seed < len(c.Inputs) && seed != i && !c.Inputs[seed].Excluded {
					chosen, next, zero := chooseZeroDecisionAlternative(c, seed, trialRoles[seed], trial, collisions)
					if !zero {
						ok = false
						break
					}
					trialRoles[seed] = []datasetDecisionRoleLabel{chosen}
					trial = next
				}
			}
			if ok {
				roles = trialRoles
				roles[i] = []datasetDecisionRoleLabel{alt}
				units = trial
				foundIneligible = true
				break
			}
		}
		if !foundIneligible {
			return datasetDecisionWorld{}, false
		}
	}
	for _, i := range zeroDirect {
		if datasetDecisionContextErr(c) != nil {
			return datasetDecisionWorld{}, false
		}
		if i < 0 || i >= len(c.Inputs) {
			return datasetDecisionWorld{}, false
		}
		if c.Inputs[i].Excluded {
			continue
		}
		chosen, next, ok := chooseZeroDecisionAlternative(c, i, roles[i], units, collisions)
		if !ok {
			return datasetDecisionWorld{}, false
		}
		roles[i], units = []datasetDecisionRoleLabel{chosen}, next
	}
	if positive != nil {
		if positive.Input < 0 || positive.Input >= len(c.Inputs) {
			return datasetDecisionWorld{}, false
		}
		if !hasDecisionAlternative(roles[positive.Input], positive.Alternative) {
			return datasetDecisionWorld{}, false
		}
		chosen, next, ok := choosePositiveDecisionAlternative(c, positive.Input, []datasetDecisionRoleLabel{positive.Alternative}, units, collisions)
		if !ok {
			return datasetDecisionWorld{}, false
		}
		roles[positive.Input], units = []datasetDecisionRoleLabel{chosen}, next
	}
	assignment := cloneDecisionUnits(units)
	w := datasetDecisionWorld{DataLabels: map[int]string{}, Companion: map[int]bool{}, SharedToken: map[uint]bool{}}
	for bit := range collisions {
		if datasetDecisionContextErr(c) != nil {
			return datasetDecisionWorld{}, false
		}
		w.SharedToken[bit] = assignment[bit]
	}
	for i, alts := range roles {
		if datasetDecisionContextErr(c) != nil {
			return datasetDecisionWorld{}, false
		}
		if len(alts) == 0 {
			return datasetDecisionWorld{}, false
		}
		chosen := alts[0]
		if chosen.Role == datasetselection.EvidenceRoleCompanion {
			w.Companion[i] = true
		} else {
			w.DataLabels[i] = chosen.Label
		}
	}
	// Zero-direct constrains direct matching; ineligible additionally excludes
	// companion fallback eligibility. Both are checked against the pre-dedup
	// evaluator state below.
	for _, i := range zeroDirect {
		if i < 0 || i >= len(c.Inputs) || c.Inputs[i].Excluded {
			continue
		}
		if decisionHasDirect(c, w, i) {
			return datasetDecisionWorld{}, false
		}
	}
	if positive != nil && !decisionHasDirect(c, w, positive.Input) {
		return datasetDecisionWorld{}, false
	}
	if validateDatasetDecisionWorld(c, w) != nil {
		return datasetDecisionWorld{}, false
	}
	if len(ineligible) != 0 {
		evaluation, err := evaluateDatasetDecisionWorld(c, w)
		if err != nil {
			return datasetDecisionWorld{}, false
		}
		for _, i := range ineligible {
			if i < 0 || i >= len(evaluation.Records) || evaluation.Records[i].Eligible {
				return datasetDecisionWorld{}, false
			}
		}
	}
	return w, true
}

func hasDecisionAlternative(alts []datasetDecisionRoleLabel, x datasetDecisionRoleLabel) bool {
	for _, a := range alts {
		if a == x {
			return true
		}
	}
	return false
}
func cloneDecisionUnits(in map[uint]bool) map[uint]bool {
	out := make(map[uint]bool, len(in))
	for k, v := range in {
		out[k] = v
	}
	return out
}

func chooseZeroDecisionAlternative(c datasetDecisionContext, i int, alts []datasetDecisionRoleLabel, units map[uint]bool, collisions map[uint]bool) (datasetDecisionRoleLabel, map[uint]bool, bool) {
	for _, role := range []datasetselection.EvidenceRole{datasetselection.EvidenceRoleData, datasetselection.EvidenceRoleCompanion} {
		for _, alt := range alts {
			if datasetDecisionContextErr(c) != nil {
				return datasetDecisionRoleLabel{}, nil, false
			}
			if alt.Role != role {
				continue
			}
			trial := cloneDecisionUnits(units)
			if constrainDecisionDirect(c, i, []datasetDecisionRoleLabel{alt}, trial, collisions, false) {
				return alt, trial, true
			}
		}
	}
	return datasetDecisionRoleLabel{}, nil, false
}

func choosePositiveDecisionAlternative(c datasetDecisionContext, i int, alts []datasetDecisionRoleLabel, units map[uint]bool, collisions map[uint]bool) (datasetDecisionRoleLabel, map[uint]bool, bool) {
	for _, alt := range alts {
		if datasetDecisionContextErr(c) != nil {
			return datasetDecisionRoleLabel{}, nil, false
		}
		trial := cloneDecisionUnits(units)
		if constrainDecisionDirect(c, i, []datasetDecisionRoleLabel{alt}, trial, collisions, true) {
			return alt, trial, true
		}
	}
	return datasetDecisionRoleLabel{}, nil, false
}

// constrainDecisionDirect handles one direct condition. A positive condition
// succeeds when an actual role has any membership or literal-path match; a
// negative condition chooses one actual role with no direct match. Callers pin
// zero/positive role choices into the returned world.
func constrainDecisionDirect(c datasetDecisionContext, i int, alts []datasetDecisionRoleLabel, units map[uint]bool, collisions map[uint]bool, want bool) bool {
	if i < 0 || i >= len(c.Inputs) || c.Inputs[i].Excluded {
		return !want
	}
	if len(alts) == 0 {
		return !want
	}
	for _, alt := range alts {
		trial := cloneDecisionUnits(units)
		ok := true
		for _, f := range c.Filters {
			if datasetDecisionContextErr(c) != nil {
				return false
			}
			matched := datasetDirectTerm(c, i, alt, f)
			if want {
				if matched {
					for k, v := range trial {
						units[k] = v
					}
					return true
				}
				// This role/filter directly matches independently of membership bits.
				continue
			}
			if matched {
				ok = false
				break
			}
		}
		if ok {
			for k, v := range trial {
				units[k] = v
			}
			return true
		}
	}
	return false
}

func isDatasetCoreBit(c datasetDecisionContext, bit uint) (string, bool) {
	for _, f := range c.Filters {
		if f.Bit == bit {
			for _, label := range datasetCoreLabels {
				if f.Normalized == label {
					return label, true
				}
			}
			return "", false
		}
	}
	return "", false
}

// datasetDirectTerm reports one actual role/filter literal-or-membership match.
func datasetDirectTerm(c datasetDecisionContext, i int, alt datasetDecisionRoleLabel, f datasetDecisionFilter) bool {
	pathMatch := filterMatchesPath(c.Inputs[i].Path, f.Original, c.ExactMatch)
	if alt.Role == datasetselection.EvidenceRoleCompanion || !containsString(c.RecognizedLabels, f.Normalized) {
		return pathMatch
	}
	// Membership is an additional reason to select, never an alternative to
	// ordinary literal path matching. In particular a recognized term can
	// collide with a different group label while also matching this path.
	return pathMatch || strings.EqualFold(alt.Label, f.Normalized)
}

func decisionHasDirect(c datasetDecisionContext, w datasetDecisionWorld, i int) bool {
	label := w.DataLabels[i]
	companion := w.Companion[i]
	recognized := make(map[string]bool, len(c.RecognizedLabels))
	for _, v := range c.RecognizedLabels {
		recognized[v] = true
	}
	for _, f := range c.Filters {
		if concreteDatasetFilterMatches(c.Inputs[i], f, c, label, companion, recognized) {
			return true
		}
	}
	return false
}

// datasetDecisionWorld is one complete concrete interpretation. SharedToken
// keys are normalized filter bits; true means membership interpretation and
// false means ordinary path interpretation. Bits are supplied only for
// compiled collisions. Production code does not enumerate these worlds.
type datasetDecisionWorld struct {
	DataLabels  map[int]string
	Companion   map[int]bool
	SharedToken map[uint]bool
}

type datasetDecisionRecord struct {
	Eligible bool
	Selected bool
	Skipped  bool
	Subdir   string
}

type datasetDecisionEvaluation struct {
	Records []datasetDecisionRecord
}

// datasetDecisionQueryResult keeps feasibility separate from its coherent
// witness. Err distinguishes an interrupted proof from a disproved proposition.
type datasetDecisionQueryResult struct {
	Possible bool
	Err      error
	World    datasetDecisionWorld
	Record   datasetDecisionRecord
}

// queryDatasetDecision proves a target's emission when requireIneligible is
// false, or a raw pre-dedup ineligibility witness when it is true. Its
// earlierCaseIneligible inputs are likewise raw eligibility constraints, not
// generic output-skip requirements. Exact/case-fold suppression is applied by
// the upper output wrappers.
func queryDatasetDecision(c datasetDecisionContext, index int, earlierCaseIneligible []int, requireIneligible bool) datasetDecisionQueryResult {
	if err := datasetDecisionContextErr(c); err != nil {
		return datasetDecisionQueryResult{Err: err}
	}
	if index < 0 || index >= len(c.Inputs) || !c.IsDataset || c.GGUFMode || len(c.Filters) == 0 {
		return datasetDecisionQueryResult{}
	}
	if !requireIneligible && len(earlierCaseIneligible) == 0 {
		if r, fixed := fixedDatasetDecisionQuery(c, index, false); fixed {
			return r
		}
	}
	if !requireIneligible && len(earlierCaseIneligible) == 0 {
		if r, unique := uniqueDatasetDecisionQuery(c, index, false); unique {
			return r
		}
	}
	for _, core := range datasetCoreAssignments(c) {
		if err := datasetDecisionContextErr(c); err != nil {
			return datasetDecisionQueryResult{Err: err}
		}
		if requireIneligible {
			w, ok := feasibleDatasetDecisionWorld(c, core, nil, []int{index}, nil, nil)
			if ok {
				e, err := evaluateDatasetDecisionWorld(c, w)
				if err == nil && !e.Records[index].Eligible {
					return datasetDecisionQueryResult{Possible: true, World: w, Record: e.Records[index]}
				}
			}
			continue
		}
		// A direct selection is witnessed by one actual data alternative and
		// one direct term, all interpreted in this same world.
		for _, alt := range c.Inputs[index].Alternatives {
			if err := datasetDecisionContextErr(c); err != nil {
				return datasetDecisionQueryResult{Err: err}
			}
			if alt.Role != datasetselection.EvidenceRoleData {
				continue
			}
			w, ok := feasibleDatasetDecisionWorld(c, core, datasetDecisionPins{index: alt}, earlierCaseIneligible, nil, &datasetDecisionPositive{Input: index, Alternative: alt})
			if ok && queryWorldEmits(c, w, index) {
				e, _ := evaluateDatasetDecisionWorld(c, w)
				return datasetDecisionQueryResult{Possible: true, World: w, Record: e.Records[index]}
			}
		}
		// A companion is retained at the root without a seed, or below a
		// directory seeded by one directly selected data input. The source and
		// target roles are pinned together so an input cannot self-seed through
		// its mutually exclusive data/companion interpretations.
		companion := datasetDecisionRoleLabel{Role: datasetselection.EvidenceRoleCompanion}
		// Companions may themselves satisfy an ordinary explicit path filter;
		// this is a direct witness, not root/nested fallback retention.
		w, ok := feasibleDatasetDecisionWorld(c, core, datasetDecisionPins{index: companion}, earlierCaseIneligible, nil, &datasetDecisionPositive{Input: index, Alternative: companion})
		if ok && queryWorldEmits(c, w, index) {
			e, _ := evaluateDatasetDecisionWorld(c, w)
			return datasetDecisionQueryResult{Possible: true, World: w, Record: e.Records[index]}
		}
		if path.Dir(strings.ReplaceAll(c.Inputs[index].Path, "\\", "/")) == "." || c.RetainMetadata && isRecognizedDatasetMetadataPath(c.Inputs[index].Path) && !c.Inputs[index].LFS {
			w, ok := feasibleDatasetDecisionWorld(c, core, datasetDecisionPins{index: companion}, earlierCaseIneligible, nil, nil)
			if ok && queryWorldEmits(c, w, index) {
				e, _ := evaluateDatasetDecisionWorld(c, w)
				return datasetDecisionQueryResult{Possible: true, World: w, Record: e.Records[index]}
			}
		}
		for _, seed := range validDatasetDirectorySeeds(c, c.Inputs[index].Directory) {
			if err := datasetDecisionContextErr(c); err != nil {
				return datasetDecisionQueryResult{Err: err}
			}
			if seed == index || c.Inputs[seed].Excluded {
				continue
			}
			for _, alt := range c.Inputs[seed].Alternatives {
				if err := datasetDecisionContextErr(c); err != nil {
					return datasetDecisionQueryResult{Err: err}
				}
				w, ok := feasibleDatasetDecisionWorld(c, core, datasetDecisionPins{index: companion, seed: alt}, earlierCaseIneligible, []int{index}, &datasetDecisionPositive{Input: seed, Alternative: alt})
				if ok && queryWorldEmits(c, w, index) {
					e, _ := evaluateDatasetDecisionWorld(c, w)
					return datasetDecisionQueryResult{Possible: true, World: w, Record: e.Records[index]}
				}
			}
		}
	}
	if err := datasetDecisionContextErr(c); err != nil {
		return datasetDecisionQueryResult{Err: err}
	}
	return datasetDecisionQueryResult{}
}

// fixedDatasetDecisionQuery proves per-record constant outcomes before using
// the exact solver. It is deliberately conservative: any unresolved role,
// token, seed, or first-eligible duplicate condition falls through.
func fixedDatasetDecisionQuery(c datasetDecisionContext, index int, requireSkip bool) (datasetDecisionQueryResult, bool) {
	if c.uniqueEvaluation == nil {
		return datasetDecisionQueryResult{}, false
	}
	e := c.uniqueEvaluation
	e.mu.Lock()
	if !e.fixedDone {
		outcomes := fixedDatasetDecisionOutcomes(c)
		if err := datasetDecisionContextErr(c); err != nil {
			e.mu.Unlock()
			return datasetDecisionQueryResult{Err: err}, true
		}
		e.fixedEmit, e.fixedDone = outcomes, true
	}
	if index >= len(e.fixedEmit) {
		e.mu.Unlock()
		return datasetDecisionQueryResult{}, true
	}
	// Only a proof of the opposite fixed outcome may answer a query here.
	// A merely possible outcome is not evidence that its complement is
	// impossible (e.g. data-vs-companion root fallback).
	fixed := e.fixedEmit[index]
	e.mu.Unlock()
	if fixed == 0 {
		return datasetDecisionQueryResult{}, false
	}
	if (fixed == 1) != !requireSkip {
		return datasetDecisionQueryResult{}, true
	}
	// Use one independently admissible concrete world as the witness. The
	// per-record proof establishes its queried outcome without relying on a
	// global unique-world proof.
	initializeDatasetDecisionBaseline(c, e)
	if err := datasetDecisionContextErr(c); err != nil {
		return datasetDecisionQueryResult{Err: err}, true
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	if !e.valid || index >= len(e.records) || e.records[index].Selected == requireSkip {
		return datasetDecisionQueryResult{}, false
	}
	return datasetDecisionQueryResult{Possible: true, World: e.world, Record: e.records[index]}, true
}

func initializeDatasetDecisionBaseline(c datasetDecisionContext, e *datasetDecisionUniqueEvaluation) {
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.baseDone {
		return
	}
	if datasetDecisionContextErr(c) != nil {
		return
	}
	{
		world := datasetDecisionWorld{DataLabels: make(map[int]string), Companion: make(map[int]bool), SharedToken: make(map[uint]bool)}
		for bit := range collisionTokenBits(c) {
			if datasetDecisionContextErr(c) != nil {
				return
			}
			world.SharedToken[bit] = false
		}
		for i, in := range c.Inputs {
			if datasetDecisionContextErr(c) != nil {
				return
			}
			if len(in.Alternatives) == 0 {
				return
			}
			alt := in.Alternatives[0]
			if alt.Role == datasetselection.EvidenceRoleCompanion {
				world.Companion[i] = true
			} else {
				world.DataLabels[i] = alt.Label
			}
		}
		e.world = world
		if validateDatasetDecisionWorld(c, world) != nil {
			return
		}
		evaluation, err := evaluateDatasetDecisionWorld(c, world)
		if err != nil {
			return
		}
		e.records, e.valid = evaluation.Records, true
		e.baseDone = true
	}
}

func fixedDatasetDecisionOutcomes(c datasetDecisionContext) []int8 {
	n := len(c.Inputs)
	collisions := collisionTokenBits(c)
	direct := make([][]int8, n) // +1 always direct, -1 never direct, 0 varies.
	allDirect, anyDirect := make([]bool, n), make([]bool, n)
	for i, in := range c.Inputs {
		if datasetDecisionContextErr(c) != nil {
			return nil
		}
		if in.Excluded || len(in.Alternatives) == 0 {
			continue
		}
		allDirect[i] = true
		direct[i] = make([]int8, len(in.Alternatives))
		for j, alt := range in.Alternatives {
			if datasetDecisionContextErr(c) != nil {
				return nil
			}
			value, fixed := invariantDatasetDirect(c, i, alt, collisions)
			if !fixed {
				direct[i][j] = 0
				allDirect[i], anyDirect[i] = false, true
			} else if value {
				direct[i][j], anyDirect[i] = 1, true
			} else {
				direct[i][j], allDirect[i] = -1, false
			}
		}
	}
	guaranteedDirs, possibleDirs := map[string]bool{}, map[string]bool{}
	for i, in := range c.Inputs {
		if datasetDecisionContextErr(c) != nil {
			return nil
		}
		if in.Excluded || len(in.Alternatives) == 0 || !anyDirect[i] {
			continue
		}
		for _, dir := range datasetDecisionSeedDirectories(in) {
			possibleDirs[dir] = true
			if allDirect[i] {
				guaranteedDirs[dir] = true
			}
		}
	}
	outcomes := make([]int8, n) // +1 always emits, -1 always skips, 0 varies/is unproved.
	seenExact, seenFold := map[string]bool{}, map[string]int8{}
	for i, in := range c.Inputs {
		if datasetDecisionContextErr(c) != nil {
			return nil
		}
		fold := strings.ToLower(in.Path)
		duplicate := seenExact[in.Path]
		seenExact[in.Path] = true
		priorOutcome, priorSeen := seenFold[fold]
		if !duplicate && !in.Excluded {
			// The concrete evaluator applies this fixed GGUF eligibility rule
			// after direct/companion selection. Keep the cached proof domain
			// identical so a root companion cannot be proven emit-capable here.
			ggufRejected := strings.HasSuffix(strings.ToLower(in.Path), ".gguf") && !datasetGGUFSelected(c, in.Path)
			allEligible, anyEligible := true, false
			for j, alt := range in.Alternatives {
				directAlways, directPossible := direct[i][j] == 1, direct[i][j] >= 0
				eligible, possible := directAlways, directPossible
				if alt.Role == datasetselection.EvidenceRoleCompanion && !in.LFS {
					dir := path.Dir(strings.ReplaceAll(in.Path, "\\", "/"))
					metadata := c.RetainMetadata && isRecognizedDatasetMetadataPath(in.Path)
					fallback := metadata || dir == "." || guaranteedDirs[dir]
					maybeFallback := metadata || dir == "." || possibleDirs[dir]
					eligible, possible = eligible || fallback, possible || maybeFallback
				}
				if ggufRejected {
					eligible, possible = false, false
				}
				if !eligible {
					allEligible = false
				}
				if possible {
					anyEligible = true
				}
			}
			if duplicate || priorSeen && priorOutcome == 1 || !anyEligible {
				outcomes[i] = -1
			} else if allEligible && (!priorSeen || priorOutcome == -1) {
				outcomes[i] = 1
			}
		}
		if in.Excluded || duplicate {
			outcomes[i] = -1
		}
		if !priorSeen || outcomes[i] == 1 || outcomes[i] == 0 && priorOutcome == -1 {
			seenFold[fold] = outcomes[i]
		}
	}
	return outcomes
}

// uniqueDatasetDecisionQuery avoids repeating whole-inventory feasibility and
// evaluation when there is one concrete role/token world, or when direct
// selection is stable across every role/token interpretation. In either case
// a baseline-world record is a valid witness. Other contexts use the exact
// feasibility solver.
func uniqueDatasetDecisionQuery(c datasetDecisionContext, index int, requireSkip bool) (datasetDecisionQueryResult, bool) {
	if c.uniqueEvaluation == nil {
		return datasetDecisionQueryResult{}, false
	}
	e := c.uniqueEvaluation
	e.mu.Lock()
	if !e.proofDone {
		unique := uniqueDatasetDecisionWorld(c) || stableDirectEvaluation(c) || stableDatasetDecisionEvaluation(c)
		if err := datasetDecisionContextErr(c); err != nil {
			e.mu.Unlock()
			return datasetDecisionQueryResult{Err: err}, true
		}
		e.unique, e.proofDone = unique, true
	}
	unique := e.unique
	e.mu.Unlock()
	if !unique {
		return datasetDecisionQueryResult{}, false
	}
	initializeDatasetDecisionBaseline(c, e)
	if err := datasetDecisionContextErr(c); err != nil {
		return datasetDecisionQueryResult{Err: err}, true
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	if !e.valid || index >= len(e.records) {
		return datasetDecisionQueryResult{}, true
	}
	record := e.records[index]
	possible := record.Skipped == requireSkip
	if !possible {
		return datasetDecisionQueryResult{}, true
	}
	return datasetDecisionQueryResult{Possible: true, World: e.world, Record: record}, true
}

// uniqueDatasetDecisionWorld preserves the cheap complete evaluation for
// singleton role alternatives without colliding path tokens. It includes
// ordinary unmatched, excluded, and implicit-companion inputs: their behavior
// is fully determined by that one concrete interpretation, not by requiring
// every input to match a filter.
func uniqueDatasetDecisionWorld(c datasetDecisionContext) bool {
	if len(collisionTokenBits(c)) != 0 {
		return false
	}
	for _, in := range c.Inputs {
		if datasetDecisionContextErr(c) != nil {
			return false
		}
		if len(in.Alternatives) != 1 {
			return false
		}
	}
	return true
}

// stableDirectEvaluation proves, without enumerating role worlds, that each
// input is directly selected under every actual role. Consequently companion
// fallback and role-dependent directory seeds are irrelevant. A false return
// is only a missed optimization.
func stableDirectEvaluation(c datasetDecisionContext) bool {
	for i, in := range c.Inputs {
		if datasetDecisionContextErr(c) != nil {
			return false
		}
		if in.Excluded || len(in.Alternatives) == 0 {
			return false
		}
		for _, alt := range in.Alternatives {
			if datasetDecisionContextErr(c) != nil {
				return false
			}
			fixedTrue := false
			for _, f := range c.Filters {
				if datasetDecisionContextErr(c) != nil {
					return false
				}
				if datasetDirectTerm(c, i, alt, f) {
					fixedTrue = true
					break
				}
			}
			if !fixedTrue {
				return false
			}
		}
	}
	return true
}

// stableDatasetDecisionEvaluation certifies a reusable complete evaluation
// when each input's direct-match result is invariant across its actual roles,
// then checks that companion fallback eligibility is invariant against the
// resulting fixed seed directories. Any direct selection seeds its directory
// regardless of role. Input order and duplicate suppression are consequently
// deterministic as well. Attribution need not be invariant: the cached records
// are evaluated in one coherent admissible baseline world, which remains a
// valid refusal witness.
func stableDatasetDecisionEvaluation(c datasetDecisionContext) bool {
	collisions := collisionTokenBits(c)
	direct := make([]bool, len(c.Inputs))
	for i, in := range c.Inputs {
		if datasetDecisionContextErr(c) != nil {
			return false
		}
		if len(in.Alternatives) == 0 {
			return false
		}
		if in.Excluded {
			continue
		}
		for altIndex, alt := range in.Alternatives {
			if datasetDecisionContextErr(c) != nil {
				return false
			}
			value, invariant := invariantDatasetDirect(c, i, alt, collisions)
			if !invariant || (altIndex > 0 && value != direct[i]) {
				return false
			}
			if altIndex == 0 {
				direct[i] = value
			}
		}
	}
	seedCounts := make(map[string]int)
	for i, in := range c.Inputs {
		if in.Excluded || !direct[i] {
			continue
		}
		for _, dir := range datasetDecisionSeedDirectories(in) {
			if datasetDecisionContextErr(c) != nil {
				return false
			}
			seedCounts[dir]++
		}
	}
	dirs := make(map[string]bool, len(seedCounts))
	for dir := range seedCounts {
		dirs[dir] = true
	}
	for i, in := range c.Inputs {
		if datasetDecisionContextErr(c) != nil {
			return false
		}
		if in.Excluded {
			continue
		}
		var eligible bool
		for altIndex, alt := range in.Alternatives {
			if datasetDecisionContextErr(c) != nil {
				return false
			}
			selected := direct[i]
			if !selected && alt.Role == datasetselection.EvidenceRoleCompanion && !in.LFS {
				dir := path.Dir(strings.ReplaceAll(in.Path, "\\", "/"))
				selected = c.RetainMetadata && isRecognizedDatasetMetadataPath(in.Path) || dir == "." || dirs[dir]
			}
			if altIndex > 0 && selected != eligible {
				return false
			}
			if altIndex == 0 {
				eligible = selected
			}
		}
	}
	return true
}

func datasetDecisionSeedDirectories(in datasetDecisionInput) []string {
	var dirs []string
	for d := path.Dir(strings.ReplaceAll(in.Path, "\\", "/")); d != "." && d != "/"; d = path.Dir(d) {
		dirs = append(dirs, d)
	}
	return dirs
}

// invariantDatasetDirect computes one role's direct selection; membership and
// path matches are independent reasons, so no token interpretation is needed.
func invariantDatasetDirect(c datasetDecisionContext, i int, alt datasetDecisionRoleLabel, _ map[uint]bool) (bool, bool) {
	for _, f := range c.Filters {
		if datasetDecisionContextErr(c) != nil {
			return false, false
		}
		if datasetDirectTerm(c, i, alt, f) {
			return true, true
		}
	}
	return false, true
}

// A directly selected file seeds its containing directory and ancestors. A
// companion in D therefore may be seeded by a source in D or below D.
func validDatasetDirectorySeeds(c datasetDecisionContext, directory string) []int {
	var indexes []int
	for seedDir, seeds := range c.DirectSeedsByDirectory {
		if datasetDecisionContextErr(c) != nil {
			return nil
		}
		if seedDir == directory || strings.HasPrefix(seedDir, strings.TrimSuffix(directory, "/")+"/") {
			indexes = append(indexes, seeds...)
		}
	}
	sort.Ints(indexes)
	return indexes
}

func datasetCoreAssignments(c datasetDecisionContext) []map[uint]bool {
	var bits []uint
	for bit := range collisionTokenBits(c) {
		if _, core := isDatasetCoreBit(c, bit); core {
			bits = append(bits, bit)
		}
	}
	sort.Slice(bits, func(i, j int) bool { return bits[i] < bits[j] })
	if len(bits) > len(datasetCoreLabels) {
		return nil
	}
	assignments := make([]map[uint]bool, 0, 1<<len(bits))
	for mask := 0; mask < 1<<len(bits); mask++ {
		values := make(map[uint]bool, len(bits))
		for i, bit := range bits {
			values[bit] = mask&(1<<i) != 0
		}
		assignments = append(assignments, values)
	}
	return assignments
}

func canSelectDatasetDecision(c datasetDecisionContext, index int, earlierCaseIneligible []int) datasetDecisionQueryResult {
	return queryDatasetDecision(c, index, earlierCaseIneligible, false)
}

func canSkipDatasetDecision(c datasetDecisionContext, index int) datasetDecisionQueryResult {
	if err := datasetDecisionContextErr(c); err != nil {
		return datasetDecisionQueryResult{Err: err}
	}
	if index < 0 || index >= len(c.Inputs) {
		return datasetDecisionQueryResult{}
	}
	// These shortcuts prove the final output proposition, so they remain valid
	// even when the reason is exact/case-fold duplicate suppression.
	if r, fixed := fixedDatasetDecisionQuery(c, index, true); fixed {
		return r
	}
	if r, unique := uniqueDatasetDecisionQuery(c, index, true); unique {
		return r
	}
	// A case-folded duplicate may be skipped because an earlier eligible
	// record emitted, even when the duplicate itself remains eligible. Ask the
	// ordered emission wrapper for that witness before asking the lower solver
	// to construct a world where this record is ineligible.
	fold := strings.ToLower(c.Inputs[index].Path)
	for _, i := range caseFoldPredecessors(c, index, fold) {
		if err := datasetDecisionContextErr(c); err != nil {
			return datasetDecisionQueryResult{Err: err}
		}
		if r := canEmitDatasetDecision(c, i); r.Possible {
			e, err := evaluateDatasetDecisionWorld(c, r.World)
			if err == nil && e.Records[index].Skipped {
				return datasetDecisionQueryResult{Possible: true, World: r.World, Record: e.Records[index]}
			}
		} else if r.Err != nil {
			return r
		}
	}
	// Exact duplicates are suppressed regardless of their raw eligibility.
	// Produce an unconstrained coherent witness rather than misreporting that
	// output fact as a lower-kernel ineligibility constraint.
	if c.Inputs[index].ExactDuplicateOf >= 0 {
		for _, core := range datasetCoreAssignments(c) {
			if err := datasetDecisionContextErr(c); err != nil {
				return datasetDecisionQueryResult{Err: err}
			}
			w, ok := feasibleDatasetDecisionWorld(c, core, nil, nil, nil, nil)
			if !ok {
				continue
			}
			e, err := evaluateDatasetDecisionWorld(c, w)
			if err == nil && e.Records[index].Skipped {
				return datasetDecisionQueryResult{Possible: true, World: w, Record: e.Records[index]}
			}
			if ctxErr := datasetDecisionContextErr(c); ctxErr != nil {
				return datasetDecisionQueryResult{Err: ctxErr}
			}
		}
		return datasetDecisionQueryResult{}
	}
	return queryDatasetDecision(c, index, nil, true)
}

func queryWorldEmits(c datasetDecisionContext, w datasetDecisionWorld, index int) bool {
	e, err := evaluateDatasetDecisionWorld(c, w)
	return err == nil && index < len(e.Records) && e.Records[index].Selected
}

func canEmitDatasetDecision(c datasetDecisionContext, index int) datasetDecisionQueryResult {
	if err := datasetDecisionContextErr(c); err != nil {
		return datasetDecisionQueryResult{Err: err}
	}
	if index < 0 || index >= len(c.Inputs) {
		return datasetDecisionQueryResult{}
	}
	in := c.Inputs[index]
	// Exact duplicates are always suppressed by input order, including when
	// the earlier record is excluded. Case-folded duplicates are suppressed
	// only after an earlier record actually becomes eligible; model that with
	// ineligibility constraints instead of the compiler's lexical-first hint.
	if !in.ExactDuplicateEligible {
		return datasetDecisionQueryResult{}
	}
	if r, fixed := fixedDatasetDecisionQuery(c, index, false); fixed {
		// A fixed proof covers both the output result and its ordered duplicate
		// handling. For a positive result, the cached concrete evaluation is the
		// witness: Record.Selected already entails that no earlier same-fold
		// record emitted. Do not rescan the inventory prefix to re-prove it.
		return r
	}
	// Every earlier same-fold exact-first record must be ineligible; if any is
	// eligible, it is the first case-fold winner and blocks this target. Exact
	// duplicates are already suppressed before eligibility and impose no such
	// constraint (but still contribute direct directory seeds in evaluation).
	var earlier []int
	// Most records have no same-fold predecessor; compiler metadata proves
	// that case without scanning the prefix. Only duplicate classes need the
	// indexed predecessor list, whose work is proportional to actual case-fold
	// edges rather than the entire inventory prefix.
	if in.CaseDuplicateOf >= 0 {
		fold := strings.ToLower(in.Path)
		for _, prior := range c.CaseIndexesByPath[fold] {
			if err := datasetDecisionContextErr(c); err != nil {
				return datasetDecisionQueryResult{Err: err}
			}
			if prior >= index {
				break
			}
			if c.Inputs[prior].ExactDuplicateOf < 0 {
				earlier = append(earlier, prior)
			}
		}
	}
	return canSelectDatasetDecision(c, index, earlier)
}

func canNotEmitDatasetDecision(c datasetDecisionContext, index int) datasetDecisionQueryResult {
	if err := datasetDecisionContextErr(c); err != nil {
		return datasetDecisionQueryResult{Err: err}
	}
	if index < 0 || index >= len(c.Inputs) {
		return datasetDecisionQueryResult{}
	}
	if r, fixed := fixedDatasetDecisionQuery(c, index, true); fixed {
		return r
	}
	if r := canSkipDatasetDecision(c, index); r.Possible || r.Err != nil {
		return r
	}
	fold := strings.ToLower(c.Inputs[index].Path)
	for _, i := range caseFoldPredecessors(c, index, fold) {
		if err := datasetDecisionContextErr(c); err != nil {
			return datasetDecisionQueryResult{Err: err}
		}
		if r := canEmitDatasetDecision(c, i); r.Possible {
			return r
		} else if r.Err != nil {
			return r
		}
	}
	return datasetDecisionQueryResult{}
}

func caseFoldPredecessors(c datasetDecisionContext, index int, fold string) []int {
	if index < 0 || index >= len(c.Inputs) || c.Inputs[index].CaseDuplicateOf < 0 {
		return nil
	}
	indexes := c.CaseIndexesByPath[fold]
	end := sort.Search(len(indexes), func(i int) bool { return indexes[i] >= index })
	return indexes[:end]
}

func datasetDecisionRefusal(c datasetDecisionContext, index int) (can, cannot datasetDecisionQueryResult) {
	can = canEmitDatasetDecision(c, index)
	if can.Possible {
		cannot = canNotEmitDatasetDecision(c, index)
	}
	return
}

// queryDatasetGroupSelection finds one coherent world in which an actual data
// record is selected by a recognized membership filter. A possible emission
// alone is insufficient: companions and metadata retention are not membership.
func queryDatasetGroupSelection(c datasetDecisionContext) datasetDecisionQueryResult {
	if err := datasetDecisionContextErr(c); err != nil {
		return datasetDecisionQueryResult{Err: err}
	}
	if !c.IsDataset || c.GGUFMode || len(c.Filters) == 0 {
		return datasetDecisionQueryResult{}
	}
	labels := make(map[string]bool, len(c.RecognizedLabels))
	for _, label := range c.RecognizedLabels {
		labels[label] = true
	}
	wanted := make(map[string]bool)
	for _, f := range c.Filters {
		if labels[f.Normalized] || !c.ExactMatch && recognizedDatasetFilter(c, f.Normalized) {
			wanted[f.Normalized] = true
		}
	}
	if len(wanted) == 0 {
		return datasetDecisionQueryResult{}
	}
	for _, core := range datasetCoreAssignments(c) {
		for i, in := range c.Inputs {
			if err := datasetDecisionContextErr(c); err != nil {
				return datasetDecisionQueryResult{Err: err}
			}
			if in.Excluded {
				continue
			}
			for _, alt := range in.Alternatives {
				if alt.Role != datasetselection.EvidenceRoleData || !wanted[strings.ToLower(alt.Label)] {
					continue
				}
				w, ok := feasibleDatasetDecisionWorld(c, core, datasetDecisionPins{i: alt}, nil, nil, nil)
				if !ok {
					continue
				}
				e, err := evaluateDatasetDecisionWorld(c, w)
				if err != nil {
					return datasetDecisionQueryResult{Err: err}
				}
				if e.Records[i].Selected {
					return datasetDecisionQueryResult{Possible: true, World: w, Record: e.Records[i]}
				}
			}
		}
	}
	if err := datasetDecisionContextErr(c); err != nil {
		return datasetDecisionQueryResult{Err: err}
	}
	return datasetDecisionQueryResult{}
}

// evaluateDatasetDecisionWorld applies the exact-dataset-group concrete
// selection contract to one admissible role/token interpretation. No-filter,
// inexact, non-dataset, and GGUF requests are outside this evaluator's
// established contract.
func evaluateDatasetDecisionWorld(c datasetDecisionContext, world datasetDecisionWorld) (datasetDecisionEvaluation, error) {
	if err := datasetDecisionContextErr(c); err != nil {
		return datasetDecisionEvaluation{}, err
	}
	if !c.IsDataset || c.GGUFMode || len(c.Filters) == 0 {
		return datasetDecisionEvaluation{}, fmt.Errorf("concrete dataset evaluator requires a filtered non-GGUF dataset request")
	}
	recognizedLabels := make(map[string]bool, len(c.RecognizedLabels))
	for _, label := range c.RecognizedLabels {
		recognizedLabels[label] = true
	}
	groupSelected := false
	for _, f := range c.Filters {
		if datasetDecisionContextErr(c) != nil {
			return datasetDecisionEvaluation{}, datasetDecisionContextErr(c)
		}
		if recognizedLabels[f.Normalized] || !c.ExactMatch && recognizedDatasetFilter(c, f.Normalized) {
			groupSelected = true
			break
		}
	}
	_ = groupSelected // recognized groups and ordinary path filters share this evaluator.
	if err := validateDatasetDecisionWorld(c, world); err != nil {
		return datasetDecisionEvaluation{}, err
	}
	out := datasetDecisionEvaluation{Records: make([]datasetDecisionRecord, len(c.Inputs))}
	dirs := make(map[string]bool)
	direct := make([]bool, len(c.Inputs))
	labels := make([]string, len(c.Inputs))
	companions := make([]bool, len(c.Inputs))
	for i, in := range c.Inputs {
		if datasetDecisionContextErr(c) != nil {
			return datasetDecisionEvaluation{}, datasetDecisionContextErr(c)
		}
		if in.Excluded {
			continue
		}
		labels[i] = strings.ToLower(world.DataLabels[i])
		companions[i] = world.Companion[i]
		for _, f := range c.Filters {
			if err := datasetDecisionContextErr(c); err != nil {
				return datasetDecisionEvaluation{}, err
			}
			if concreteDatasetFilterMatches(in, f, c, labels[i], companions[i], recognizedLabels) {
				direct[i] = true
			}
		}
		if direct[i] {
			for d := path.Dir(strings.ReplaceAll(in.Path, "\\", "/")); d != "." && d != "/"; d = path.Dir(d) {
				dirs[d] = true
			}
		}
	}
	seenExact, seenFold := make(map[string]bool), make(map[string]bool)
	for i, in := range c.Inputs {
		if err := datasetDecisionContextErr(c); err != nil {
			return datasetDecisionEvaluation{}, err
		}
		r := &out.Records[i]
		eligible := !in.Excluded && (direct[i] || (companions[i] && !in.LFS && (c.RetainMetadata && isRecognizedDatasetMetadataPath(in.Path) || path.Dir(strings.ReplaceAll(in.Path, "\\", "/")) == "." || dirs[path.Dir(strings.ReplaceAll(in.Path, "\\", "/"))])))
		if strings.HasSuffix(strings.ToLower(in.Path), ".gguf") && !datasetGGUFSelected(c, in.Path) {
			eligible = false
		}
		r.Eligible = eligible
		if seenExact[in.Path] {
			r.Skipped = true
			continue
		}
		seenExact[in.Path] = true
		if in.Excluded {
			r.Skipped = true
			continue
		}
		if seenFold[strings.ToLower(in.Path)] {
			r.Skipped = true
			continue
		}
		if !eligible {
			r.Skipped = true
			continue
		}
		// Attribution considers every applicable direct term, preserves input
		// order for equal lengths, and never turns attribution ambiguity into a
		// selection error.
		bestLength := -1
		for _, f := range c.Filters {
			if err := datasetDecisionContextErr(c); err != nil {
				return datasetDecisionEvaluation{}, err
			}
			if concreteDatasetFilterMatches(in, f, c, labels[i], companions[i], recognizedLabels) && len(f.Original) > bestLength {
				r.Subdir, bestLength = f.Original, len(f.Original)
			}
		}
		r.Selected = true
		seenFold[strings.ToLower(in.Path)] = true
	}
	return out, nil
}

func datasetGGUFSelected(c datasetDecisionContext, filePath string) bool {
	for _, filter := range c.Filters {
		if filterMatchesPath(filePath, filter.Original, c.ExactMatch) {
			return true
		}
	}
	return false
}

func recognizedDatasetFilter(c datasetDecisionContext, filter string) bool {
	if filter == "" {
		return false
	}
	for _, label := range c.RecognizedLabels {
		if strings.Contains(label, filter) {
			return true
		}
	}
	return false
}

func datasetDecisionContextErr(c datasetDecisionContext) error {
	if c.ctx == nil {
		return nil
	}
	return c.ctx.Err()
}

func concreteDatasetFilterMatches(in datasetDecisionInput, f datasetDecisionFilter, c datasetDecisionContext, label string, companion bool, recognizedLabels map[string]bool) bool {
	if companion {
		// Ordinary path selection remains available to companions.
		return filterMatchesPath(in.Path, f.Original, c.ExactMatch)
	}
	pathMatch := filterMatchesPath(in.Path, f.Original, c.ExactMatch)
	if recognizedLabels[f.Normalized] {
		return pathMatch || label != "" && label == f.Normalized
	}
	return pathMatch
}

func validateDatasetDecisionWorld(c datasetDecisionContext, world datasetDecisionWorld) error {
	for i := range world.DataLabels {
		if i < 0 || i >= len(c.Inputs) {
			return fmt.Errorf("data role refers to unknown input %d", i)
		}
	}
	for i := range world.Companion {
		if i < 0 || i >= len(c.Inputs) {
			return fmt.Errorf("companion role refers to unknown input %d", i)
		}
	}
	for i, in := range c.Inputs {
		if err := datasetDecisionContextErr(c); err != nil {
			return err
		}
		label, data := world.DataLabels[i]
		companion := world.Companion[i]
		if data == companion { // both false or both true
			return fmt.Errorf("input %d must have exactly one concrete role", i)
		}
		valid := false
		for _, alt := range in.Alternatives {
			if data && alt.Role == datasetselection.EvidenceRoleData && strings.EqualFold(alt.Label, label) || companion && alt.Role == datasetselection.EvidenceRoleCompanion {
				valid = true
				break
			}
		}
		if !valid {
			return fmt.Errorf("input %d role/label is not an actual membership alternative", i)
		}
	}
	collidingTokens := collisionTokenBits(c)
	for bit := range world.SharedToken {
		if !collidingTokens[bit] {
			return fmt.Errorf("shared-token interpretation supplied for non-colliding bit %d", bit)
		}
	}
	for _, f := range c.Filters {
		if !collidingTokens[f.Bit] {
			continue
		}
		if _, ok := world.SharedToken[f.Bit]; !ok {
			return fmt.Errorf("missing shared-token interpretation for filter %q", f.Normalized)
		}
	}
	return nil
}

func collisionTokenBits(c datasetDecisionContext) map[uint]bool {
	bits := make(map[uint]bool)
	filterBits := make(map[int]uint, len(c.Filters))
	for _, f := range c.Filters {
		if datasetDecisionContextErr(c) != nil {
			return bits
		}
		filterBits[f.Index] = f.Bit
	}
	for _, in := range c.Inputs {
		if datasetDecisionContextErr(c) != nil {
			return bits
		}
		if in.Excluded {
			continue
		}
		for _, index := range in.CollisionFilterIndexes {
			if bit, ok := filterBits[index]; ok {
				bits[bit] = true
			}
		}
	}
	return bits
}

// datasetDecisionWorldFromAlternatives is a strict test/construction helper:
// singleton evidence is filled automatically, but ambiguous inputs require an
// explicit actual alternative and every shared collision bit is required.
func datasetDecisionWorldFromAlternatives(c datasetDecisionContext, choices map[int]datasetDecisionRoleLabel, shared map[uint]bool) (datasetDecisionWorld, error) {
	w := datasetDecisionWorld{DataLabels: make(map[int]string), Companion: make(map[int]bool), SharedToken: make(map[uint]bool)}
	for i := range choices {
		if i < 0 || i >= len(c.Inputs) {
			return datasetDecisionWorld{}, fmt.Errorf("role choice refers to unknown input %d", i)
		}
	}
	for bit, value := range shared {
		w.SharedToken[bit] = value
	}
	for i, in := range c.Inputs {
		choice, ok := choices[i]
		if !ok && len(in.Alternatives) == 1 {
			choice, ok = in.Alternatives[0], true
		}
		if !ok {
			return datasetDecisionWorld{}, fmt.Errorf("input %d has ambiguous evidence and no explicit choice", i)
		}
		if choice.Role == datasetselection.EvidenceRoleCompanion {
			w.Companion[i] = true
		} else if choice.Role == datasetselection.EvidenceRoleData {
			w.DataLabels[i] = strings.ToLower(choice.Label)
		} else {
			return datasetDecisionWorld{}, fmt.Errorf("input %d has unsupported role %q", i, choice.Role)
		}
	}
	return w, validateDatasetDecisionWorld(c, w)
}
