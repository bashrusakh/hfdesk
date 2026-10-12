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
// the only positive conditions needed by the dormant evaluator.
type datasetDecisionPositive struct {
	Input       int
	Alternative datasetDecisionRoleLabel
}

// datasetDecisionPins restricts inputs to actual compiled alternatives.
type datasetDecisionPins map[int]datasetDecisionRoleLabel

type datasetDecisionLiteral struct {
	Bit   uint
	Value bool
}

// feasibleDatasetDecisionWorld finds one coherent concrete role/token world
// satisfying pins, required skips, zero-direct constraints and (when present)
// one positive direct clause. It is dormant and does not activate planning.
func feasibleDatasetDecisionWorld(c datasetDecisionContext, coreAssignment map[uint]bool, pins datasetDecisionPins, requiredSkips, zeroDirect []int, positive *datasetDecisionPositive) (datasetDecisionWorld, bool) {
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
	for _, i := range requiredSkips {
		if i < 0 || i >= len(c.Inputs) {
			return datasetDecisionWorld{}, false
		}
		in := c.Inputs[i]
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
		// A nested data alternative with no direct match is skipped regardless
		// of companion-directory scope, so prefer that explanation first.
		dataAlternatives := make([]datasetDecisionRoleLabel, 0, len(roles[i]))
		foundSkip := false
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
				foundSkip = true
				break
			}
		}
		if !foundSkip {
			return datasetDecisionWorld{}, false
		}
	}
	for _, i := range zeroDirect {
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
		w.SharedToken[bit] = assignment[bit]
	}
	for i, alts := range roles {
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
	for _, i := range append(append([]int(nil), zeroDirect...), requiredSkips...) {
		if i < 0 || i >= len(c.Inputs) || c.Inputs[i].Excluded {
			continue
		}
		if decisionHasDirect(c, w, i, collisions) {
			return datasetDecisionWorld{}, false
		}
	}
	if positive != nil && !decisionHasDirect(c, w, positive.Input, collisions) {
		return datasetDecisionWorld{}, false
	}
	if validateDatasetDecisionWorld(c, w) != nil {
		return datasetDecisionWorld{}, false
	}
	if len(requiredSkips) != 0 {
		evaluation, err := evaluateDatasetDecisionWorld(c, w)
		if err != nil {
			return datasetDecisionWorld{}, false
		}
		for _, i := range requiredSkips {
			if i < 0 || i >= len(evaluation.Records) || !evaluation.Records[i].Skipped {
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
		trial := cloneDecisionUnits(units)
		if constrainDecisionDirect(c, i, []datasetDecisionRoleLabel{alt}, trial, collisions, true) {
			return alt, trial, true
		}
	}
	return datasetDecisionRoleLabel{}, nil, false
}

// The compiled matcher/domain gives this reduction its bounded shape: a
// recognized colliding data term is either one membership literal, one
// ordinary-path literal, or both complementary literals for the same token;
// a non-colliding recognized term is constant by role, and companions or
// unrecognized filters are constant ordinary-path terms. Thus zero-direct is
// a conjunction of per-filter zero clauses: each clause is a unit assignment
// or impossible (including complementary literals), and shared assignments
// are unified in units. The five conventional groups plus default contribute
// at most six colliding core bits are supplied as a fixed assignment by the
// caller; residual labels use only these units and deterministic false
// defaults. This proof depends on datasetselection's
// actual-label alternatives and the concrete filter matcher, not arbitrary
// future role/token schemas.
//
// constrainDecisionDirect handles one direct condition. A positive condition
// is a disjunction across filters/actual role alternatives; a negative
// condition chooses one zero-direct actual role and forces every filter term
// false. Callers pin zero/positive role choices into the returned world.
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
			literals, constant := datasetDirectTerm(c, i, alt, f, collisions)
			if want {
				if constant {
					for k, v := range trial {
						units[k] = v
					}
					return true
				}
				for _, lit := range literals {
					if v, fixed := trial[lit.Bit]; !fixed || v == lit.Value {
						trial[lit.Bit] = lit.Value
						// This alternative/filter witnesses the positive clause.
						for k, v := range trial {
							units[k] = v
						}
						return true
					}
				}
				continue
			}
			if constant {
				ok = false
				break
			}
			// To make this OR-of-literals term false, every literal must be
			// assigned its opposite. Repeated bits naturally unify in the map.
			for _, lit := range literals {
				if v, fixed := trial[lit.Bit]; fixed && v == lit.Value {
					ok = false
					break
				}
				trial[lit.Bit] = !lit.Value
			}
			if !ok {
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

// datasetDirectTerm compiles one actual role/filter term. Its literals are
// precisely the shared-token interpretation choices that make the concrete
// evaluator's term true; constant marks an unconditional direct match.
func datasetDirectTerm(c datasetDecisionContext, i int, alt datasetDecisionRoleLabel, f datasetDecisionFilter, collisions map[uint]bool) ([]datasetDecisionLiteral, bool) {
	pathMatch := filterMatchesPath(c.Inputs[i].Path, f.Original, c.ExactMatch)
	if alt.Role == datasetselection.EvidenceRoleCompanion || !containsString(c.RecognizedLabels, f.Normalized) {
		return nil, pathMatch
	}
	if !collisions[f.Bit] {
		return nil, strings.EqualFold(alt.Label, f.Normalized)
	}
	var literals []datasetDecisionLiteral
	if strings.EqualFold(alt.Label, f.Normalized) {
		literals = append(literals, datasetDecisionLiteral{f.Bit, true})
	}
	if pathMatch {
		literals = append(literals, datasetDecisionLiteral{f.Bit, false})
	}
	return literals, false
}

func decisionHasDirect(c datasetDecisionContext, w datasetDecisionWorld, i int, collisions map[uint]bool) bool {
	label := w.DataLabels[i]
	companion := w.Companion[i]
	recognized := make(map[string]bool, len(c.RecognizedLabels))
	for _, v := range c.RecognizedLabels {
		recognized[v] = true
	}
	for _, f := range c.Filters {
		if concreteDatasetFilterMatches(c.Inputs[i], f, c, w, label, companion, collisions, recognized) {
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
	Selected bool
	Skipped  bool
	Subdir   string
}

type datasetDecisionEvaluation struct {
	Records []datasetDecisionRecord
}

// datasetDecisionQueryResult keeps feasibility separate from its coherent
// witness. A nil witness means the proposition was disproved, not unknown.
type datasetDecisionQueryResult struct {
	Possible bool
	World    datasetDecisionWorld
	Record   datasetDecisionRecord
}

// queryDatasetDecision runs bounded concrete queries over the six conventional
// collision bits. Residual token bits are solved by the unit primitive rather
// than enumerated. It intentionally has no caller in PlanRepo yet.
func queryDatasetDecision(c datasetDecisionContext, index int, earlierCaseSkips []int, requireSkip bool) datasetDecisionQueryResult {
	if index < 0 || index >= len(c.Inputs) || !c.IsDataset || !c.ExactMatch || c.GGUFMode || len(c.Filters) == 0 {
		return datasetDecisionQueryResult{}
	}
	if r, unique := uniqueDatasetDecisionQuery(c, index, requireSkip); unique {
		return r
	}
	for _, core := range datasetCoreAssignments(c) {
		if requireSkip {
			w, ok := feasibleDatasetDecisionWorld(c, core, nil, []int{index}, nil, nil)
			if ok {
				e, err := evaluateDatasetDecisionWorld(c, w)
				if err == nil && e.Records[index].Skipped {
					return datasetDecisionQueryResult{Possible: true, World: w, Record: e.Records[index]}
				}
			}
			continue
		}
		// A direct selection is witnessed by one actual data alternative and
		// one direct term, all interpreted in this same world.
		for _, alt := range c.Inputs[index].Alternatives {
			if alt.Role != datasetselection.EvidenceRoleData {
				continue
			}
			w, ok := feasibleDatasetDecisionWorld(c, core, datasetDecisionPins{index: alt}, earlierCaseSkips, nil, &datasetDecisionPositive{Input: index, Alternative: alt})
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
		w, ok := feasibleDatasetDecisionWorld(c, core, datasetDecisionPins{index: companion}, earlierCaseSkips, nil, &datasetDecisionPositive{Input: index, Alternative: companion})
		if ok && queryWorldEmits(c, w, index) {
			e, _ := evaluateDatasetDecisionWorld(c, w)
			return datasetDecisionQueryResult{Possible: true, World: w, Record: e.Records[index]}
		}
		if path.Dir(strings.ReplaceAll(c.Inputs[index].Path, "\\", "/")) == "." {
			w, ok := feasibleDatasetDecisionWorld(c, core, datasetDecisionPins{index: companion}, earlierCaseSkips, nil, nil)
			if ok && queryWorldEmits(c, w, index) {
				e, _ := evaluateDatasetDecisionWorld(c, w)
				return datasetDecisionQueryResult{Possible: true, World: w, Record: e.Records[index]}
			}
		}
		for _, seed := range validDatasetDirectorySeeds(c, c.Inputs[index].Directory) {
			if seed == index || c.Inputs[seed].Excluded {
				continue
			}
			for _, alt := range c.Inputs[seed].Alternatives {
				if alt.Role != datasetselection.EvidenceRoleData {
					continue
				}
				w, ok := feasibleDatasetDecisionWorld(c, core, datasetDecisionPins{index: companion, seed: alt}, earlierCaseSkips, []int{index}, &datasetDecisionPositive{Input: seed, Alternative: alt})
				if ok && queryWorldEmits(c, w, index) {
					e, _ := evaluateDatasetDecisionWorld(c, w)
					return datasetDecisionQueryResult{Possible: true, World: w, Record: e.Records[index]}
				}
			}
		}
	}
	return datasetDecisionQueryResult{}
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
	e.proofOnce.Do(func() {
		e.unique = uniqueDatasetDecisionWorld(c) || stableDirectEvaluation(c) || stableDatasetDecisionEvaluation(c)
	})
	if !e.unique {
		return datasetDecisionQueryResult{}, false
	}
	e.once.Do(func() {
		world := datasetDecisionWorld{DataLabels: make(map[int]string), Companion: make(map[int]bool), SharedToken: make(map[uint]bool)}
		for bit := range collisionTokenBits(c) {
			world.SharedToken[bit] = false
		}
		for i, in := range c.Inputs {
			alt := in.Alternatives[0]
			if alt.Role == datasetselection.EvidenceRoleCompanion {
				world.Companion[i] = true
			} else {
				world.DataLabels[i] = alt.Label
			}
		}
		e.world = world
		if err := validateDatasetDecisionWorld(c, world); err != nil {
			return
		}
		evaluation, err := evaluateDatasetDecisionWorld(c, world)
		if err != nil {
			return
		}
		e.records, e.valid = evaluation.Records, true
	})
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
		if len(in.Alternatives) != 1 {
			return false
		}
	}
	return true
}

// stableDirectEvaluation proves, without enumerating role/token worlds, that
// each input is directly selected under every actual role and shared-token
// assignment. Consequently companion fallback and role-dependent directory
// seeds are irrelevant. A false return is only a missed optimization.
func stableDirectEvaluation(c datasetDecisionContext) bool {
	collisions := collisionTokenBits(c)
	for i, in := range c.Inputs {
		if in.Excluded || len(in.Alternatives) == 0 {
			return false
		}
		for _, alt := range in.Alternatives {
			fixedTrue := false
			seenLiterals := make(map[uint]bool)
			for _, f := range c.Filters {
				literals, constant := datasetDirectTerm(c, i, alt, f, collisions)
				if constant {
					fixedTrue = true
					break
				}
				for _, literal := range literals {
					if value, ok := seenLiterals[literal.Bit]; ok && value != literal.Value {
						fixedTrue = true
						break
					}
					seenLiterals[literal.Bit] = literal.Value
				}
				if fixedTrue {
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
// when each input's direct-match result is invariant across its actual roles
// and every shared-token assignment. It also requires the direct data-seed
// status to be role-invariant, then checks that companion fallback eligibility
// is invariant against the resulting fixed seed directories. Input order and
// duplicate suppression are consequently deterministic as well. Attribution
// need not be invariant: the cached records are evaluated in one coherent
// admissible baseline world, which remains a valid refusal witness.
func stableDatasetDecisionEvaluation(c datasetDecisionContext) bool {
	collisions := collisionTokenBits(c)
	direct := make([]bool, len(c.Inputs))
	for i, in := range c.Inputs {
		if len(in.Alternatives) == 0 {
			return false
		}
		if in.Excluded {
			continue
		}
		for altIndex, alt := range in.Alternatives {
			value, invariant := invariantDatasetDirect(c, i, alt, collisions)
			if !invariant || (altIndex > 0 && value != direct[i]) {
				return false
			}
			if altIndex == 0 {
				direct[i] = value
			}
		}
	}
	seedDirsByInput := make([][]string, len(c.Inputs))
	seedCounts := make(map[string]int)
	for i, in := range c.Inputs {
		if in.Excluded || !direct[i] || c.Inputs[i].Alternatives[0].Role != datasetselection.EvidenceRoleData {
			continue
		}
		seedDirsByInput[i] = datasetDecisionSeedDirectories(in)
		for _, dir := range seedDirsByInput[i] {
			seedCounts[dir]++
		}
	}
	for i, in := range c.Inputs {
		if in.Excluded {
			continue
		}
		for _, alt := range in.Alternatives {
			var candidateDirs []string
			if direct[i] && alt.Role == datasetselection.EvidenceRoleData {
				candidateDirs = datasetDecisionSeedDirectories(in)
			}
			candidateSet := make(map[string]bool, len(candidateDirs))
			for _, dir := range candidateDirs {
				candidateSet[dir] = true
				if seedCounts[dir] == 0 {
					return false
				}
			}
			for _, dir := range seedDirsByInput[i] {
				if seedCounts[dir] == 1 && !candidateSet[dir] {
					return false
				}
			}
		}
	}
	dirs := make(map[string]bool, len(seedCounts))
	for dir := range seedCounts {
		dirs[dir] = true
	}
	for i, in := range c.Inputs {
		if in.Excluded {
			continue
		}
		var eligible bool
		for altIndex, alt := range in.Alternatives {
			selected := direct[i]
			if !selected && alt.Role == datasetselection.EvidenceRoleCompanion && !in.LFS {
				dir := path.Dir(strings.ReplaceAll(in.Path, "\\", "/"))
				selected = dir == "." || dirs[dir]
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

// invariantDatasetDirect determines whether the OR of one role's filter terms
// is fixed for all shared-token assignments. A direct term is either constant,
// one literal, or (for a collision) the complementary membership/path pair;
// therefore only a constant-true term or both polarities of one shared bit
// make the disjunction tautological. An empty disjunction is false.
func invariantDatasetDirect(c datasetDecisionContext, i int, alt datasetDecisionRoleLabel, collisions map[uint]bool) (bool, bool) {
	seen := make(map[uint]bool)
	for _, f := range c.Filters {
		literals, constant := datasetDirectTerm(c, i, alt, f, collisions)
		if constant {
			return true, true
		}
		for _, literal := range literals {
			if value, ok := seen[literal.Bit]; ok && value != literal.Value {
				return true, true
			}
			seen[literal.Bit] = literal.Value
		}
	}
	if len(seen) == 0 {
		return false, true
	}
	return false, false
}

// A directly selected file seeds its containing directory and ancestors. A
// companion in D therefore may be seeded by a source in D or below D.
func validDatasetDirectorySeeds(c datasetDecisionContext, directory string) []int {
	var indexes []int
	for seedDir, seeds := range c.DirectSeedsByDirectory {
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

func canSelectDatasetDecision(c datasetDecisionContext, index int, earlierCaseSkips []int) datasetDecisionQueryResult {
	return queryDatasetDecision(c, index, earlierCaseSkips, false)
}

func canSkipDatasetDecision(c datasetDecisionContext, index int) datasetDecisionQueryResult {
	return queryDatasetDecision(c, index, nil, true)
}

func queryWorldEmits(c datasetDecisionContext, w datasetDecisionWorld, index int) bool {
	e, err := evaluateDatasetDecisionWorld(c, w)
	return err == nil && index < len(e.Records) && e.Records[index].Selected
}

func canEmitDatasetDecision(c datasetDecisionContext, index int) datasetDecisionQueryResult {
	if index < 0 || index >= len(c.Inputs) {
		return datasetDecisionQueryResult{}
	}
	in := c.Inputs[index]
	// Exact duplicates are always suppressed by input order, including when
	// the earlier record is excluded. Case-folded duplicates are suppressed
	// only after an earlier record actually becomes eligible; model that with
	// required-skip constraints instead of the compiler's lexical-first hint.
	if !in.ExactDuplicateEligible {
		return datasetDecisionQueryResult{}
	}
	var earlier []int
	fold := strings.ToLower(in.Path)
	for i := 0; i < index; i++ {
		if strings.ToLower(c.Inputs[i].Path) == fold {
			earlier = append(earlier, i)
		}
	}
	return canSelectDatasetDecision(c, index, earlier)
}

func canNotEmitDatasetDecision(c datasetDecisionContext, index int) datasetDecisionQueryResult {
	if index < 0 || index >= len(c.Inputs) {
		return datasetDecisionQueryResult{}
	}
	if r := canSkipDatasetDecision(c, index); r.Possible {
		return r
	}
	fold := strings.ToLower(c.Inputs[index].Path)
	for i := 0; i < index; i++ {
		if strings.ToLower(c.Inputs[i].Path) == fold {
			if r := canEmitDatasetDecision(c, i); r.Possible {
				return r
			}
		}
	}
	return datasetDecisionQueryResult{}
}

func datasetDecisionRefusal(c datasetDecisionContext, index int) (can, cannot datasetDecisionQueryResult) {
	can = canEmitDatasetDecision(c, index)
	if can.Possible {
		cannot = canNotEmitDatasetDecision(c, index)
	}
	return
}

// evaluateDatasetDecisionWorld applies the exact-dataset-group concrete
// selection contract to one admissible role/token interpretation. It remains
// dormant: PlanRepo does not call it. No-filter, inexact, non-dataset, and
// GGUF requests are outside this evaluator's established contract.
func evaluateDatasetDecisionWorld(c datasetDecisionContext, world datasetDecisionWorld) (datasetDecisionEvaluation, error) {
	if !c.IsDataset || !c.ExactMatch || c.GGUFMode || len(c.Filters) == 0 {
		return datasetDecisionEvaluation{}, fmt.Errorf("concrete dataset evaluator requires an exact filtered non-GGUF dataset request")
	}
	recognizedLabels := make(map[string]bool, len(c.RecognizedLabels))
	for _, label := range c.RecognizedLabels {
		recognizedLabels[label] = true
	}
	groupSelected := false
	for _, f := range c.Filters {
		if recognizedLabels[f.Normalized] {
			groupSelected = true
			break
		}
	}
	if !groupSelected {
		return datasetDecisionEvaluation{}, fmt.Errorf("concrete dataset evaluator requires at least one recognized group filter")
	}
	if err := validateDatasetDecisionWorld(c, world); err != nil {
		return datasetDecisionEvaluation{}, err
	}
	collidingTokens := collisionTokenBits(c)
	out := datasetDecisionEvaluation{Records: make([]datasetDecisionRecord, len(c.Inputs))}
	dirs := make(map[string]bool)
	direct := make([]bool, len(c.Inputs))
	labels := make([]string, len(c.Inputs))
	companions := make([]bool, len(c.Inputs))
	for i, in := range c.Inputs {
		if in.Excluded {
			continue
		}
		labels[i] = strings.ToLower(world.DataLabels[i])
		companions[i] = world.Companion[i]
		for _, f := range c.Filters {
			if concreteDatasetFilterMatches(in, f, c, world, labels[i], companions[i], collidingTokens, recognizedLabels) {
				direct[i] = true
			}
		}
		if direct[i] && !companions[i] {
			for d := path.Dir(strings.ReplaceAll(in.Path, "\\", "/")); d != "." && d != "/"; d = path.Dir(d) {
				dirs[d] = true
			}
		}
	}
	seenExact, seenFold := make(map[string]bool), make(map[string]bool)
	for i, in := range c.Inputs {
		r := &out.Records[i]
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
		if !direct[i] {
			// Data never falls back to companion retention. Only an actual
			// companion role can enter the root/directory fallback, and LFS
			// companions are intentionally not retained by that fallback.
			if !companions[i] || in.LFS {
				r.Skipped = true
				continue
			}
			dir := path.Dir(strings.ReplaceAll(in.Path, "\\", "/"))
			if dir != "." && !dirs[dir] {
				r.Skipped = true
				continue
			}
		}
		// Attribution considers every applicable direct term, preserves input
		// order for equal lengths, and never turns attribution ambiguity into a
		// selection error.
		bestLength := -1
		for _, f := range c.Filters {
			if concreteDatasetFilterMatches(in, f, c, world, labels[i], companions[i], collidingTokens, recognizedLabels) && len(f.Original) > bestLength {
				r.Subdir, bestLength = f.Original, len(f.Original)
			}
		}
		r.Selected = true
		seenFold[strings.ToLower(in.Path)] = true
	}
	return out, nil
}

func concreteDatasetFilterMatches(in datasetDecisionInput, f datasetDecisionFilter, c datasetDecisionContext, world datasetDecisionWorld, label string, companion bool, collidingTokens map[uint]bool, recognizedLabels map[string]bool) bool {
	if companion {
		// Ordinary path selection remains available to companions, including
		// recognized group-token matches in either shared interpretation.
		return filterMatchesPath(in.Path, f.Original, c.ExactMatch)
	}
	if recognizedLabels[f.Normalized] {
		if collidingTokens[f.Bit] && !world.SharedToken[f.Bit] {
			return filterMatchesPath(in.Path, f.Original, c.ExactMatch)
		}
		return label != "" && label == f.Normalized
	}
	return filterMatchesPath(in.Path, f.Original, c.ExactMatch)
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
		filterBits[f.Index] = f.Bit
	}
	for _, in := range c.Inputs {
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
