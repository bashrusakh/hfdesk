// Copyright 2025
// SPDX-License-Identifier: Apache-2.0

package smartdl

import (
	"strings"
	"testing"
)

// qwythosMTPFiles mirrors empero-ai/Qwythos-9B-Claude-Mythos-5-1M-GGUF, a real
// repo that ships, for several quantization levels, both a plain quant and an
// MTP (Multi-Token Prediction) variant that carries the same quant token. The
// analyzer must treat each MTP file as its own quantization rather than merging
// it with the non-MTP twin (which produced one row with a bogus summed size).
func qwythosMTPFiles() []FileInfo {
	specs := []struct {
		name string
		size int64
	}{
		{"Qwythos-9B-Claude-Mythos-5-1M-BF16.gguf", 17920697344},
		{"Qwythos-9B-Claude-Mythos-5-1M-MTP-BF16.gguf", 18407321536},
		{"Qwythos-9B-Claude-Mythos-5-1M-Q4_K_M.gguf", 5629109248},
		{"Qwythos-9B-Claude-Mythos-5-1M-MTP-Q4_K_M.gguf", 5887668160},
		{"Qwythos-9B-Claude-Mythos-5-1M-Q5_K_M.gguf", 6467970048},
		{"Qwythos-9B-Claude-Mythos-5-1M-MTP-Q5_K_M.gguf", 6726528960},
		{"Qwythos-9B-Claude-Mythos-5-1M-Q6_K.gguf", 7359259648},
		{"Qwythos-9B-Claude-Mythos-5-1M-MTP-Q6_K.gguf", 7617818560},
		{"Qwythos-9B-Claude-Mythos-5-1M-Q8_0.gguf", 9527501824},
		{"Qwythos-9B-Claude-Mythos-5-1M-MTP-Q8_0.gguf", 9786060736},
		{"mmproj-Qwythos-9B-Claude-Mythos-5-1M-F16.gguf", 918165472},
	}
	files := make([]FileInfo, 0, len(specs))
	for _, s := range specs {
		files = append(files, FileInfo{Name: s.name, Path: s.name, Size: s.size})
	}
	return files
}

func TestHasMTPMarker(t *testing.T) {
	tests := []struct {
		name string
		want bool
	}{
		{"Qwythos-9B-Claude-Mythos-5-1M-MTP-Q4_K_M.gguf", true},
		{"Qwythos-9B-Claude-Mythos-5-1M-MTP-BF16.gguf", true},
		{"model-Q4_K_M-MTP.gguf", true},
		{"model_mtp_Q4_K_M.gguf", true},
		{"Qwythos-9B-Claude-Mythos-5-1M-Q4_K_M.gguf", false},
		{"Qwythos-9B-Claude-Mythos-5-1M-BF16.gguf", false},
		// "mtp" must be a whole delimiter-bounded segment, not a substring.
		{"some-mtproj-Q4_K_M.gguf", false},
		{"emptmp-Q4_K_M.gguf", false},
	}
	for _, tt := range tests {
		if got := hasMTPMarker(tt.name); got != tt.want {
			t.Errorf("hasMTPMarker(%q) = %v, want %v", tt.name, got, tt.want)
		}
	}
}

// TestAnalyzeGGUFKeepsMTPSeparate is the core regression test: each quant level
// must yield two distinct rows (plain + MTP), each carrying only its own file's
// size — never the sum of the twin pair.
func TestAnalyzeGGUFKeepsMTPSeparate(t *testing.T) {
	info := analyzeGGUF(qwythosMTPFiles())
	if info == nil {
		t.Fatal("analyzeGGUF returned nil")
	}

	// Index quantizations by (token, isMTP).
	type key struct {
		name string
		mtp  bool
	}
	bySig := make(map[key]GGUFQuantization)
	for _, q := range info.Quantizations {
		k := key{q.Name, q.IsMTP}
		if _, dup := bySig[k]; dup {
			t.Errorf("duplicate quantization signature %+v", k)
		}
		bySig[k] = q
	}

	// Every quant token should have both a plain and an MTP entry, each with its
	// own (un-summed) size.
	wantSizes := map[key]int64{
		{"BF16", false}:   17920697344,
		{"BF16", true}:    18407321536,
		{"Q4_K_M", false}: 5629109248,
		{"Q4_K_M", true}:  5887668160,
		{"Q5_K_M", false}: 6467970048,
		{"Q5_K_M", true}:  6726528960,
		{"Q6_K", false}:   7359259648,
		{"Q6_K", true}:    7617818560,
		{"Q8_0", false}:   9527501824,
		{"Q8_0", true}:    9786060736,
	}
	if len(info.Quantizations) != len(wantSizes) {
		t.Fatalf("got %d quantizations, want %d: %+v",
			len(info.Quantizations), len(wantSizes), info.Quantizations)
	}
	for k, want := range wantSizes {
		q, ok := bySig[k]
		if !ok {
			t.Errorf("missing quantization %+v", k)
			continue
		}
		if q.File.Size != want {
			t.Errorf("quant %+v size = %d, want %d (twin was likely merged)",
				k, q.File.Size, want)
		}
		if q.Parts != 1 {
			t.Errorf("quant %+v Parts = %d, want 1", k, q.Parts)
		}
	}
}

// TestGGUFToSelectableItemsMTP verifies the picker shows distinct rows with
// unique download filters, so selecting a plain quant never also pulls its MTP
// twin (and vice versa).
func TestGGUFToSelectableItemsMTP(t *testing.T) {
	info := analyzeGGUF(qwythosMTPFiles())
	items := GGUFToSelectableItems(info)

	type row struct {
		label  string
		filter string
	}
	var q4plain, q4mtp *row
	seenIDs := make(map[string]bool)
	for _, it := range items {
		if it.Category != "quantization" {
			continue
		}
		if seenIDs[it.ID] {
			t.Errorf("duplicate SelectableItem ID %q", it.ID)
		}
		seenIDs[it.ID] = true

		r := row{it.Label, it.FilterValue}
		switch it.ID {
		case "q4_k_m":
			q4plain = &r
		case "q4_k_m-mtp":
			q4mtp = &r
		}

		// No quant filter may be a substring-ambiguous bare token when MTP twins
		// exist: each filter must match exactly one of the two twin files.
		matches := 0
		for _, f := range qwythosMTPFiles() {
			if strings.Contains(strings.ToLower(f.Name), it.FilterValue) {
				matches++
			}
		}
		if matches != 1 {
			t.Errorf("filter %q for %q matched %d files, want exactly 1",
				it.FilterValue, it.Label, matches)
		}
	}

	if q4plain == nil || q4mtp == nil {
		t.Fatalf("expected both q4_k_m and q4_k_m-mtp rows; got plain=%v mtp=%v",
			q4plain, q4mtp)
	}
	if q4plain.label != "Q4_K_M" {
		t.Errorf("plain label = %q, want Q4_K_M", q4plain.label)
	}
	if q4mtp.label != "Q4_K_M (MTP)" {
		t.Errorf("MTP label = %q, want \"Q4_K_M (MTP)\"", q4mtp.label)
	}
	if q4plain.filter == q4mtp.filter {
		t.Errorf("plain and MTP share filter %q; must be distinct", q4plain.filter)
	}
}
