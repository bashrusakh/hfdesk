// Copyright 2025
// SPDX-License-Identifier: Apache-2.0

package smartdl

import (
	"reflect"
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
		{"model MTP Q4_K_M.gguf", true},
		{"model\tMTP\tQ4_K_M.gguf", true},
		{"model\u00a0MTP\u00a0Q4_K_M.gguf", true},
		{"Qwythos-9B-Claude-Mythos-5-1M-Q4_K_M.gguf", false},
		{"Qwythos-9B-Claude-Mythos-5-1M-BF16.gguf", false},
		// "mtp" must be a whole delimiter-bounded segment, not a substring.
		{"some-mtproj-Q4_K_M.gguf", false},
		{"emptmp-Q4_K_M.gguf", false},
		{"model MTPQ4_K_M.gguf", false},
		{"some mtproj Q4_K_M.gguf", false},
	}
	for _, tt := range tests {
		if got := hasMTPMarker(tt.name); got != tt.want {
			t.Errorf("hasMTPMarker(%q) = %v, want %v", tt.name, got, tt.want)
		}
	}
}

// TestAnalyzeGGUFWhitespaceDelimitedMTPMarker covers a repo where the MTP
// marker is separated by whitespace instead of the usual hyphens. The larger
// MTP artifact must remain a distinct non-recommended option, while the plain
// twin is the sole recommendation.
func TestAnalyzeGGUFWhitespaceDelimitedMTPMarker(t *testing.T) {
	const (
		mtpPath   = "model MTP Q4_K_M.gguf"
		plainPath = "model Q4_K_M.gguf"
	)
	info := analyzeGGUF([]FileInfo{
		{Name: mtpPath, Path: mtpPath, Size: 5_000_000_000},
		{Name: plainPath, Path: plainPath, Size: 4_000_000_000},
	})
	if info == nil {
		t.Fatal("analyzeGGUF returned nil")
	}
	if got := len(info.Quantizations); got != 2 {
		t.Fatalf("got %d quantizations, want 2: %+v", got, info.Quantizations)
	}

	var mtp, plain *GGUFQuantization
	for i := range info.Quantizations {
		q := &info.Quantizations[i]
		switch {
		case q.IsMTP:
			mtp = q
		case q.Name == "Q4_K_M":
			plain = q
		}
	}
	if mtp == nil {
		t.Fatalf("whitespace-delimited MTP artifact was not classified IsMTP: %+v", info.Quantizations)
	}
	if got, want := mtp.Files, []string{mtpPath}; !reflect.DeepEqual(got, want) {
		t.Fatalf("MTP files = %v, want %v", got, want)
	}
	if mtp.File.Size != 5_000_000_000 {
		t.Fatalf("MTP size = %d, want its own 5000000000 bytes", mtp.File.Size)
	}
	if plain == nil {
		t.Fatalf("plain Q4_K_M artifact missing: %+v", info.Quantizations)
	}
	if plain.IsMTP {
		t.Fatalf("plain artifact was classified IsMTP: %+v", plain)
	}
	if got, want := plain.Files, []string{plainPath}; !reflect.DeepEqual(got, want) {
		t.Fatalf("plain files = %v, want %v", got, want)
	}

	items := selectableItemsByCategory(GGUFToSelectableItems(info), "quantization")
	if len(items) != 2 {
		t.Fatalf("got %d quantization items, want 2: %+v", len(items), items)
	}
	var recommended []SelectableItem
	for _, item := range items {
		if strings.Contains(item.Label, "MTP") && item.Recommended {
			t.Errorf("MTP item %q must not be auto-recommended", item.Label)
		}
		if item.Recommended {
			recommended = append(recommended, item)
		}
	}
	if len(recommended) != 1 {
		t.Fatalf("got %d recommended quantizations, want exactly 1: %+v", len(recommended), recommended)
	}
	if got, want := recommended[0].Files, []string{plainPath}; !reflect.DeepEqual(got, want) {
		t.Fatalf("recommended files = %v, want the non-MTP twin %v", got, want)
	}
}

// TestAnalyzeGGUFWhitespaceMTPShardsKeepCompleteFiles covers the interaction
// between whitespace-delimited MTP markers and split grouping: the shards must
// merge into one MTP artifact with every part in Files, and the non-MTP twin
// must remain the sole recommendation.
func TestAnalyzeGGUFWhitespaceMTPShardsKeepCompleteFiles(t *testing.T) {
	mtpShards := []string{
		"model MTP Q4_K_M 00001-of-00002.gguf",
		"model MTP Q4_K_M 00002-of-00002.gguf",
	}
	const plainPath = "model Q4_K_M.gguf"
	info := analyzeGGUF([]FileInfo{
		{Name: mtpShards[0], Path: mtpShards[0], Size: 2_500_000_000},
		{Name: mtpShards[1], Path: mtpShards[1], Size: 2_500_000_000},
		{Name: plainPath, Path: plainPath, Size: 4_000_000_000},
	})
	if info == nil {
		t.Fatal("analyzeGGUF returned nil")
	}
	if got := len(info.Quantizations); got != 2 {
		t.Fatalf("got %d quantizations, want 2: %+v", got, info.Quantizations)
	}
	var mtp *GGUFQuantization
	for i := range info.Quantizations {
		if info.Quantizations[i].IsMTP {
			mtp = &info.Quantizations[i]
		}
	}
	if mtp == nil {
		t.Fatalf("whitespace-delimited MTP shards were not classified IsMTP: %+v", info.Quantizations)
	}
	if got, want := mtp.Files, mtpShards; !reflect.DeepEqual(got, want) {
		t.Fatalf("MTP shard Files = %v, want complete group %v", got, want)
	}
	if mtp.Parts != 2 || mtp.File.Size != 5_000_000_000 {
		t.Fatalf("MTP parts/size = %d/%d, want 2/5000000000", mtp.Parts, mtp.File.Size)
	}
	var recommended []SelectableItem
	for _, item := range selectableItemsByCategory(GGUFToSelectableItems(info), "quantization") {
		if item.Recommended {
			recommended = append(recommended, item)
		}
	}
	if len(recommended) != 1 {
		t.Fatalf("got %d recommended quantizations, want exactly 1: %+v", len(recommended), recommended)
	}
	if got, want := recommended[0].Files, []string{plainPath}; !reflect.DeepEqual(got, want) {
		t.Fatalf("recommended files = %v, want the non-MTP twin %v", got, want)
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
