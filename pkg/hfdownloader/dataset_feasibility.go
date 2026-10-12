// Copyright 2025
// SPDX-License-Identifier: Apache-2.0

package hfdownloader

import (
	"fmt"
	"path"
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
			for _, seed := range in.AncestorDirectSeedIndexes {
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
