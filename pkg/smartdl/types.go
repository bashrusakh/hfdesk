// Copyright 2025
// SPDX-License-Identifier: Apache-2.0

// Package smartdl provides intelligent repository analysis and download assistance
// for HuggingFace Hub repositories.
//
// The Smart Downloader analyzes repositories to determine their type (GGUF, Transformers,
// Diffusers, LoRA, etc.) and presents users with intelligent download options based on
// the repository structure.
//
// Example usage:
//
//	analyzer := smartdl.NewAnalyzer(smartdl.AnalyzerOptions{
//	    Token:    os.Getenv("HF_TOKEN"),
//	    Endpoint: "https://huggingface.co",
//	})
//
//	info, err := analyzer.Analyze(ctx, "TheBloke/Mistral-7B-Instruct-v0.2-GGUF", false)
//	if err != nil {
//	    log.Fatal(err)
//	}
//
//	fmt.Printf("Type: %s\n", info.Type)
//	if info.Type == smartdl.TypeGGUF {
//	    gguf := info.GGUF
//	    for _, q := range gguf.Quantizations {
//	        fmt.Printf("  %s: %s (%s RAM)\n", q.Name, q.File.SizeHuman, q.EstimatedRAMHuman)
//	    }
//	}
package smartdl

import (
	"strings"
	"time"
)

// RepoType identifies the type of HuggingFace repository.
type RepoType string

const (
	// TypeGGUF indicates a GGUF quantized model (llama.cpp, Ollama, vLLM).
	TypeGGUF RepoType = "gguf"

	// TypeTransformers indicates a standard transformers model (safetensors/bin).
	TypeTransformers RepoType = "transformers"

	// TypeDiffusers indicates a diffusers pipeline model (Stable Diffusion, SDXL, Flux).
	TypeDiffusers RepoType = "diffusers"

	// TypeLoRA indicates a LoRA/PEFT adapter.
	TypeLoRA RepoType = "lora"

	// TypeGPTQ indicates a GPTQ quantized model.
	TypeGPTQ RepoType = "gptq"

	// TypeAWQ indicates an AWQ quantized model.
	TypeAWQ RepoType = "awq"

	// TypeONNX indicates an ONNX model.
	TypeONNX RepoType = "onnx"

	// TypeDataset indicates a HuggingFace dataset.
	TypeDataset RepoType = "dataset"

	// TypeAudio indicates an audio model (ASR, TTS, etc.).
	TypeAudio RepoType = "audio"

	// TypeVision indicates a vision model (classification, detection, etc.).
	TypeVision RepoType = "vision"

	// TypeMultimodal indicates a multimodal model (VLM, etc.).
	TypeMultimodal RepoType = "multimodal"

	// TypeGeneric indicates an unknown or generic repository type.
	TypeGeneric RepoType = "generic"
)

// String returns the string representation of a RepoType.
func (t RepoType) String() string {
	return string(t)
}

// Description returns a human-readable description of the repo type.
func (t RepoType) Description() string {
	switch t {
	case TypeGGUF:
		return "GGUF quantized model (llama.cpp, Ollama)"
	case TypeTransformers:
		return "Transformers model (safetensors)"
	case TypeDiffusers:
		return "Diffusers pipeline (Stable Diffusion, SDXL, Flux)"
	case TypeLoRA:
		return "LoRA/PEFT adapter"
	case TypeGPTQ:
		return "GPTQ quantized model"
	case TypeAWQ:
		return "AWQ quantized model"
	case TypeONNX:
		return "ONNX model"
	case TypeDataset:
		return "HuggingFace dataset"
	case TypeAudio:
		return "Audio model (ASR, TTS)"
	case TypeVision:
		return "Vision model"
	case TypeMultimodal:
		return "Multimodal model (VLM)"
	default:
		return "Generic repository"
	}
}

// FileInfo represents a file in the repository.
type FileInfo struct {
	Path string `json:"path"`

	// Name is the filename (basename of path).
	Name string `json:"name"`

	// Size is the file size in bytes.
	Size int64 `json:"size"`

	SizeHuman string `json:"size_human"`

	IsLFS bool `json:"is_lfs"`

	// SHA256 is the file hash (for LFS files).
	SHA256 string `json:"sha256,omitempty"`

	Directory string `json:"directory,omitempty"`
}

// RepoRef represents a branch or tag in the repository.
type RepoRef struct {
	Name string `json:"name"`

	// Type is "branch" or "tag".
	Type string `json:"type"`

	// Commit is the commit SHA this ref points to.
	Commit string `json:"commit,omitempty"`
}

// RepoInfo contains analyzed information about a HuggingFace repository.
type RepoInfo struct {
	// Repo is the repository ID in "owner/name" format.
	Repo string `json:"repo"`

	IsDataset bool `json:"is_dataset"`

	Type RepoType `json:"type"`

	TypeDescription string `json:"type_description"`

	Files []FileInfo `json:"files"`

	// TotalSize is the sum of all file sizes in bytes.
	TotalSize int64 `json:"total_size"`

	TotalSizeHuman string `json:"total_size_human"`

	FileCount int `json:"file_count"`

	// Commit is the resolved commit SHA.
	Commit string `json:"commit,omitempty"`

	Branch string `json:"branch,omitempty"`

	Refs []RepoRef `json:"refs,omitempty"`

	AnalyzedAt time.Time `json:"analyzed_at"`

	// Metadata contains raw parsed metadata from config files.
	// Keys depend on repo type (e.g., "config.json", "model_index.json").
	Metadata map[string]interface{} `json:"metadata,omitempty"`

	// Type-specific information (only one will be populated based on Type)
	GGUF         *GGUFInfo         `json:"gguf,omitempty"`
	Transformers *TransformersInfo `json:"transformers,omitempty"`
	Diffusers    *DiffusersInfo    `json:"diffusers,omitempty"`
	LoRA         *LoRAInfo         `json:"lora,omitempty"`
	Quantized    *QuantizedInfo    `json:"quantized,omitempty"`
	Dataset      *DatasetInfo      `json:"dataset,omitempty"`
	Audio        *AudioInfo        `json:"audio,omitempty"`
	Vision       *VisionInfo       `json:"vision,omitempty"`
	Multimodal   *MultimodalInfo   `json:"multimodal,omitempty"`
	ONNX         *ONNXInfo         `json:"onnx,omitempty"`

	// SelectableItems provides unified download options across all model types.
	// For GGUF: quantizations, for Diffusers: components/variants, for Transformers: formats, etc.
	SelectableItems []SelectableItem `json:"selectable_items,omitempty"`

	RecommendedFilters []string `json:"recommended_filters,omitempty"`

	// RecommendedDownload is a ready-to-send request body for /api/download
	// using the default/recommended items from this repository. A selection
	// targets exactly one repository, so recommended items whose files live in
	// a different repository (UpstreamRepo != "") are excluded. Those items
	// remain individually selectable through SelectableItems, where UpstreamRepo
	// identifies the repository their Files paths belong to.
	RecommendedDownload *DownloadSelection `json:"recommended_download,omitempty"`

	RelatedDownloads []RelatedDownload `json:"related_downloads,omitempty"`

	// AnalysisWarnings reports bounded, non-fatal analysis work that could not
	// be completed. The repository analysis remains usable; callers may present
	// the warning and offer the indicated user-triggered retry.
	AnalysisWarnings []AnalysisWarning `json:"analysis_warnings,omitempty"`
}

// AnalysisWarning is a non-fatal limitation encountered while analyzing a
// repository. Causes are stable, non-secret reason codes rather than raw
// upstream errors, response bodies, or URLs.
type AnalysisWarning struct {
	Code      string   `json:"code"`
	Message   string   `json:"message"`
	Causes    []string `json:"causes,omitempty"`
	Retryable bool     `json:"retryable"`
}

// ModelChainEntry is one node in the provenance chain from base model to the
// analyzed repo. The chain is ordered root-first (oldest ancestor at index 0,
// current repo at the last index).
type ModelChainEntry struct {
	// Repo is the "owner/name" repository identifier.
	Repo string `json:"repo"`

	// Relation describes how this node relates to its parent in the chain:
	// "finetune", "quantized", "merge", "adapter", or "" for the root.
	Relation string `json:"relation,omitempty"`

	// IsCurrent marks the repo that was analyzed (always the last entry).
	IsCurrent bool `json:"is_current,omitempty"`
}

// GGUFInfo contains GGUF-specific analysis results.
type GGUFInfo struct {
	// ModelName is the base model name extracted from filenames.
	ModelName string `json:"model_name,omitempty"`

	// ParameterCount is the detected parameter count (e.g., "7B", "13B").
	ParameterCount string `json:"parameter_count,omitempty"`

	Quantizations []GGUFQuantization `json:"quantizations"`

	// MMProjFiles is the list of multimodal projector (vision-encoder) files
	// that must accompany an LLM quantization download for vision-language
	// models. Populated when .gguf files whose basename contains "mmproj"
	// are present in the repo (e.g. gemma-3, llava, qwen2.5-vl in GGUF form),
	// in any of the known naming conventions: a leading "mmproj-", the
	// dot-separated ".mmproj-", and the "-mmproj" suffix forms. See
	// IsMMProjFile, which owns that rule.
	// For repos without local mmproj, this may be populated from an upstream
	// base-model repo discovered via the HF model card metadata.
	MMProjFiles []FileInfo `json:"mmproj_files,omitempty"`

	// MMProjUpstreamRepo is the "owner/name" of the repo from which mmproj
	// files were sourced when they are not present in the current repo.
	// Empty when mmproj files come from the current repo itself.
	MMProjUpstreamRepo string `json:"mmproj_upstream_repo,omitempty"`

	// ModelChain is the provenance chain from the oldest known ancestor down
	// to the current repo. Built from HF model card base_model metadata.
	// Only populated when the repo declares a base_model in its card data.
	ModelChain []ModelChainEntry `json:"model_chain,omitempty"`
}

// GGUFQuantization represents a single GGUF quantization option.
type GGUFQuantization struct {
	Name string `json:"name"`

	File FileInfo `json:"file"`

	// Quality is the quality rating (1-5 stars).
	Quality int `json:"quality"`

	QualityStars string `json:"quality_stars"`

	// EstimatedRAM is the estimated RAM needed in bytes.
	EstimatedRAM int64 `json:"estimated_ram"`

	EstimatedRAMHuman string `json:"estimated_ram_human"`

	Description string `json:"description,omitempty"`

	// Recommended indicates if this quantization is recommended for the user's system.
	Recommended bool `json:"recommended,omitempty"`

	// Parts is the number of files that make up this quantization. For
	// sharded/split GGUFs (e.g. model-Q4_K_M-00001-of-00003.gguf) this is the
	// shard count; for single-file quants it is 1.
	Parts int `json:"parts,omitempty"`

	// Files holds the repo-relative paths of every file in this quantization
	// (all shards for a split GGUF). Used so the downloader pulls the whole set.
	Files []string `json:"files,omitempty"`

	// IsMTP reports whether this quantization is a Multi-Token Prediction
	// variant — a full model that bakes in an MTP head, named with an "-MTP-"
	// segment (e.g. model-MTP-Q4_K_M.gguf). It shares a quantization token with
	// its non-MTP twin but is a distinct, separately downloadable option, so it
	// must not be merged into the same row as the plain quant.
	IsMTP bool `json:"is_mtp,omitempty"`
}

// DiffusersInfo contains Diffusers-specific analysis results.
type DiffusersInfo struct {
	// PipelineType is the pipeline class name (e.g., "StableDiffusionXLPipeline").
	PipelineType string `json:"pipeline_type"`

	PipelineDescription string `json:"pipeline_description,omitempty"`

	// DiffusersVersion is the diffusers library version from model_index.json.
	DiffusersVersion string `json:"diffusers_version,omitempty"`

	Components []DiffusersComponent `json:"components"`

	// Variants is the list of available precision variants (fp16, fp32, bf16).
	Variants []string `json:"variants,omitempty"`

	Precisions []string `json:"precisions,omitempty"`
}

// DiffusersComponent represents a component in a diffusers pipeline.
type DiffusersComponent struct {
	Name string `json:"name"`

	Library string `json:"library,omitempty"`

	ClassName string `json:"class_name,omitempty"`

	// Size is the total size of this component's files.
	Size int64 `json:"size"`

	SizeHuman string `json:"size_human"`

	Files []FileInfo `json:"files,omitempty"`

	Required bool `json:"required"`
}

// LoRAInfo contains LoRA/adapter-specific analysis results.
type LoRAInfo struct {
	// AdapterType is the adapter type (e.g., "lora", "qlora", "ia3").
	AdapterType string `json:"adapter_type"`

	AdapterDescription string `json:"adapter_description,omitempty"`

	// BaseModel is the base model this adapter is trained for.
	BaseModel string `json:"base_model,omitempty"`

	// Rank is the LoRA rank (r parameter).
	Rank int `json:"rank,omitempty"`

	// Alpha is the LoRA alpha scaling factor.
	Alpha float64 `json:"alpha,omitempty"`

	// Dropout is the LoRA dropout rate.
	Dropout float64 `json:"dropout,omitempty"`

	TargetModules []string `json:"target_modules,omitempty"`

	// Bias is the bias training mode ("none", "all", "lora_only").
	Bias string `json:"bias,omitempty"`

	// TaskType is the PEFT task type (e.g., "CAUSAL_LM").
	TaskType string `json:"task_type,omitempty"`

	// FanInFanOut indicates if fan_in_fan_out is enabled.
	FanInFanOut bool `json:"fan_in_fan_out,omitempty"`

	// InitLoraWeights indicates if LoRA weights are initialized.
	InitLoraWeights bool `json:"init_lora_weights,omitempty"`

	// QuantType is the quantization type for QLoRA (e.g., "nf4").
	QuantType string `json:"quant_type,omitempty"`
}

// QuantizedInfo contains GPTQ/AWQ-specific analysis results.
type QuantizedInfo struct {
	// Method is the quantization method (e.g., "gptq", "awq", "exl2").
	Method string `json:"method"`

	MethodDescription string `json:"method_description,omitempty"`

	// Bits is the declared bit width of the quantized tensors when the config
	// declares uniform precision and was read completely enough to establish
	// it. It stays 0 when MixedPrecision or ConfigPartial is true: a header
	// bits field then describes only part of the model or uniformity is not
	// established, and it must not be read as model-wide precision (the
	// literal value remains in Metadata). When ExcludedModules is non-empty it
	// is not a model-wide width either: present the excluded modules
	// alongside it. Because Bits is an integer, fractional or out-of-range
	// declarations remain in Metadata and are not rounded into this field.
	Bits int `json:"bits"`

	// MixedPrecision indicates observed differing declared bit widths. Width
	// declarations with no observed conflict, including incomplete declarations,
	// do not alone prove mixed precision. HeadBits and ExpertBitsMin/Max describe
	// declared groups and do not describe every tensor.
	MixedPrecision bool `json:"mixed_precision,omitempty"`

	// ConfigPartial indicates the quantization config exceeded the metadata
	// size bound and only its leading fields were recovered. Precision
	// declarations past the cut are unknown, so no model-wide uniform
	// precision is claimed from the recovered fields alone.
	ConfigPartial bool `json:"config_partial,omitempty"`

	// HeadBits is the bit width the config declares for head tensors
	// (EXL3 head_bits): a tensor-group width, not model-wide precision.
	HeadBits float64 `json:"head_bits,omitempty"`

	// ExpertBitsMin and ExpertBitsMax bound the per-expert bit widths declared
	// in the config (expert_bits / routed_expert_bits), which vary per expert.
	// Both are 0 when those declarations were cut before their widths were read.
	ExpertBitsMin float64 `json:"expert_bits_min,omitempty"`
	ExpertBitsMax float64 `json:"expert_bits_max,omitempty"`

	GroupSize int `json:"group_size,omitempty"`

	// DescAct indicates if GPTQ desc_act is enabled.
	DescAct bool `json:"desc_act,omitempty"`

	Symmetric bool `json:"symmetric,omitempty"`

	// ZeroPoint indicates if zero-point quantization is used (AWQ).
	ZeroPoint bool `json:"zero_point,omitempty"`

	Version string `json:"version,omitempty"`

	// BitsPerWeight is the EXL2 bits per weight.
	BitsPerWeight float64 `json:"bits_per_weight,omitempty"`

	// ExcludedModules is the list of modules not quantized
	// (modules_to_not_convert). When non-empty, part of the model is not
	// quantized at Bits's width, so Bits is not a model-wide width and these
	// modules must be presented alongside it. Exclusions alone are not mixed
	// precision.
	ExcludedModules []string `json:"excluded_modules,omitempty"`

	// Backends is the list of compatible inference backends.
	Backends []string `json:"backends,omitempty"`

	// ModelArchitecture is the base model architecture.
	ModelArchitecture string `json:"model_architecture,omitempty"`

	// BaseModel is the base model this was quantized from.
	BaseModel string `json:"base_model,omitempty"`

	// EstimatedVRAM is the estimated VRAM needed for inference.
	EstimatedVRAM int64 `json:"estimated_vram,omitempty"`

	EstimatedVRAMHuman string `json:"estimated_vram_human,omitempty"`
}

// DatasetInfo contains dataset-specific analysis results.
type DatasetInfo struct {
	Splits []DatasetSplit `json:"splits"`

	Configs []string `json:"configs,omitempty"`

	Formats []string `json:"formats,omitempty"`

	// PrimaryFormat is the recommended format to download.
	PrimaryFormat string `json:"primary_format,omitempty"`
}

// DatasetSplit represents a dataset split (train, test, etc.).
type DatasetSplit struct {
	// Name is the split name (e.g., "train", "test", "validation").
	Name string `json:"name"`

	Files []FileInfo `json:"files,omitempty"`

	FileCount int `json:"file_count"`

	// Size is the total size of this split.
	Size int64 `json:"size"`

	SizeHuman string `json:"size_human"`
}

// AudioInfo contains audio model-specific analysis results.
type AudioInfo struct {
	// Task is the audio task (e.g., "automatic-speech-recognition", "text-to-speech").
	Task string `json:"task"`

	TaskDescription string `json:"task_description,omitempty"`

	FeatureExtractorType string `json:"feature_extractor_type,omitempty"`

	// SampleRate is the expected audio sample rate in Hz.
	SampleRate int `json:"sample_rate,omitempty"`

	// NumMelBins is the number of mel filterbank bins (for speech models).
	NumMelBins int `json:"num_mel_bins,omitempty"`

	// MaxLength is the maximum audio length in samples or seconds.
	MaxLength int `json:"max_length,omitempty"`

	// Languages is the list of supported languages (for multilingual models).
	Languages []string `json:"languages,omitempty"`

	// Framework is the model framework (e.g., "transformers", "speechbrain").
	Framework string `json:"framework,omitempty"`
}

// VisionInfo contains vision model-specific analysis results.
type VisionInfo struct {
	// Task is the vision task (e.g., "image-classification", "object-detection").
	Task string `json:"task"`

	TaskDescription string `json:"task_description,omitempty"`

	ImageProcessorType string `json:"image_processor_type,omitempty"`

	// ImageSize is the expected input image size.
	ImageSize ImageSize `json:"image_size,omitempty"`

	// NumChannels is the number of input channels (typically 3 for RGB).
	NumChannels int `json:"num_channels,omitempty"`

	// NumLabels is the number of output classes (for classification).
	NumLabels int `json:"num_labels,omitempty"`

	// Normalization contains mean and std for image normalization.
	Normalization *ImageNormalization `json:"normalization,omitempty"`

	Framework string `json:"framework,omitempty"`
}

// ImageSize represents image dimensions.
type ImageSize struct {
	Height int `json:"height"`
	Width  int `json:"width"`
}

// ImageNormalization contains normalization parameters.
type ImageNormalization struct {
	Mean []float64 `json:"mean"`
	Std  []float64 `json:"std"`
}

// SelectableItem represents an item that can be selected for download.
// This provides a unified interface for GGUF quantizations, Diffusers components,
// Transformers weight formats, dataset splits, etc.
type SelectableItem struct {
	// ID is a unique identifier for this item (e.g., "q4_k_m", "fp16", "train").
	ID string `json:"id"`

	Label string `json:"label"`

	Description string `json:"description,omitempty"`

	// Size is the item size in bytes.
	Size int64 `json:"size,omitempty"`

	SizeHuman string `json:"size_human,omitempty"`

	// Quality is the quality rating (1-5 stars, 0 if not applicable).
	Quality int `json:"quality,omitempty"`

	QualityStars string `json:"quality_stars,omitempty"`

	Recommended bool `json:"recommended,omitempty"`

	// Category groups items (e.g., "quantization", "variant", "component", "split", "format").
	Category string `json:"category,omitempty"`

	// FilterValue is the legacy filter/display token. For GGUF selections, use
	// Files as the exact artifact identity; the token may be shared by distinct
	// artifacts and remains for compatibility with existing consumers.
	FilterValue string `json:"filter_value"`

	Files []string `json:"files,omitempty"`

	// RAM is the estimated RAM needed (for GGUF quantizations).
	RAM int64 `json:"ram,omitempty"`

	RAMHuman string `json:"ram_human,omitempty"`

	// UpstreamRepo is the "owner/name" of a different repository from which
	// this item's files should be downloaded. Set only for vision_encoder items
	// sourced from an upstream base-model repo (i.e. when MMProjUpstreamRepo is
	// set). Empty means the download targets the current repo.
	UpstreamRepo string `json:"upstream_repo,omitempty"`

	// Revision is the branch/revision for a branch-backed model option. When
	// set, selection is by revision rather than by Files or FilterValue; an
	// unfiltered download includes the branch's model support files.
	Revision string `json:"revision,omitempty"`
	// BranchVariant marks an analyzer-recognized EXL2/EXL3 branch choice. It is
	// separate from Revision so ordinary revision-backed downloads retain their
	// existing storage behavior.
	BranchVariant bool `json:"branch_variant,omitempty"`

	// Method, BitsPerWeight, and HeadBits describe branch-backed EXL variants.
	// BitsPerWeight is the bitrate named by the branch or declared in its
	// quantization config; HeadBits is a tensor-group width, not a model-wide
	// precision declaration.
	Method        string  `json:"method,omitempty"`
	BitsPerWeight float64 `json:"bits_per_weight,omitempty"`
	HeadBits      float64 `json:"head_bits,omitempty"`
}

// RelatedDownload represents a related repository that may be needed.
// For example, LoRA adapters require a base model.
type RelatedDownload struct {
	// Type identifies the relationship (e.g., "base_model", "vae", "lora").
	Type string `json:"type"`

	// Repo is the HuggingFace repository ID.
	Repo string `json:"repo"`

	Label string `json:"label"`

	Description string `json:"description,omitempty"`

	// Required indicates if this is required for the main download to work.
	Required bool `json:"required,omitempty"`

	// Size is the estimated size if known.
	Size int64 `json:"size,omitempty"`

	SizeHuman string `json:"size_human,omitempty"`
}

// DownloadSelection is the web/API representation of a repo download choice.
type DownloadSelection struct {
	Repo          string   `json:"repo"`
	Dataset       bool     `json:"dataset,omitempty"`
	Revision      string   `json:"revision,omitempty"`
	Filters       []string `json:"filters,omitempty"`
	ExactMatch    bool     `json:"exactMatch,omitempty"`
	BranchVariant bool     `json:"branchVariant,omitempty"`
}

// RecommendedFilterValues returns filters for recommended selectable items.
//
// The result is intended for one DownloadSelection targeting r.Repo. Items
// sourced from another repository (UpstreamRepo != "") are omitted: their
// Files paths are relative to that upstream repository and cannot be fetched
// by a request for r.Repo. Those items stay individually selectable with their
// UpstreamRepo and exact Files intact. If no recommended item belongs to the
// analyzed repository, the result is empty.
func (r *RepoInfo) RecommendedFilterValues() []string {
	var recommended []string
	for _, item := range r.SelectableItems {
		if item.Recommended && item.UpstreamRepo == "" && item.Revision != "" {
			// A revision-backed choice selects an entire branch, so filters from
			// any other recommended row must not leak into that request.
			return nil
		}
	}
	for _, item := range r.SelectableItems {
		if !item.Recommended {
			continue
		}
		if item.UpstreamRepo != "" {
			continue
		}
		if item.Revision != "" {
			// Branch-backed choices select a complete branch, not a path or
			// legacy filter token.
			continue
		}
		if item.Category == "quantization" || item.Category == "vision_encoder" {
			for _, file := range item.Files {
				recommended = append(recommended, exactArtifactFilter(file))
			}
			if len(item.Files) == 0 {
				recommended = append(recommended, item.FilterValue)
			}
		} else {
			recommended = append(recommended, item.FilterValue)
		}
	}
	return recommended
}

// exactArtifactFilter encodes a raw repository path for ExactMatch filters.
// Slash-containing paths already have literal path semantics; a leading ./ is
// the additive, unambiguous spelling for a literal root basename.
func exactArtifactFilter(repoPath string) string {
	if repoPath != "" && !strings.Contains(repoPath, "/") {
		return "./" + repoPath
	}
	return repoPath
}

// PopulateDownloadSelection sets recommended download fields for the web UI.
func (r *RepoInfo) PopulateDownloadSelection() {
	r.RecommendedFilters = r.RecommendedFilterValues()
	var recommendedRevision string
	var recommendedBranchVariant bool
	for _, item := range r.SelectableItems {
		if item.Recommended && item.UpstreamRepo == "" && item.Revision != "" {
			recommendedRevision = item.Revision
			recommendedBranchVariant = item.BranchVariant
			break
		}
	}

	r.RecommendedDownload = &DownloadSelection{
		Repo:          r.Repo,
		Dataset:       r.IsDataset,
		Filters:       r.RecommendedFilters,
		ExactMatch:    r.IsDataset || r.Type == TypeGGUF,
		BranchVariant: recommendedBranchVariant,
	}
	if r.Branch != "" && r.Branch != "main" {
		r.RecommendedDownload.Revision = r.Branch
	}
	if recommendedRevision != "" {
		r.RecommendedDownload.Revision = recommendedRevision
	}
	if len(r.RecommendedFilters) == 0 && recommendedRevision == "" {
		r.RecommendedDownload = nil
	}
}

// GetSelectedSize calculates the total size of selected items.
func (r *RepoInfo) GetSelectedSize(selectedIDs []string) int64 {
	idMap := make(map[string]bool)
	for _, id := range selectedIDs {
		idMap[id] = true
	}

	var total int64
	for _, item := range r.SelectableItems {
		if idMap[item.ID] {
			total += item.Size
		}
	}
	return total
}

// MultimodalInfo contains multimodal model-specific analysis results.
type MultimodalInfo struct {
	// Task is the multimodal task (e.g., "visual-question-answering", "image-to-text").
	Task string `json:"task"`

	TaskDescription string `json:"task_description,omitempty"`

	// Modalities is the list of supported modalities (e.g., ["text", "image"]).
	Modalities []string `json:"modalities"`

	ProcessorType string `json:"processor_type,omitempty"`

	VisionEncoder string `json:"vision_encoder,omitempty"`

	TextEncoder string `json:"text_encoder,omitempty"`

	// ImageSize is the expected input image size.
	ImageSize ImageSize `json:"image_size,omitempty"`

	MaxTextLength int `json:"max_text_length,omitempty"`

	Framework string `json:"framework,omitempty"`
}

// ONNXInfo contains ONNX model-specific analysis results.
type ONNXInfo struct {
	Models []ONNXModel `json:"models"`

	Optimized bool `json:"optimized,omitempty"`

	Quantized bool `json:"quantized,omitempty"`

	// Runtimes is the list of compatible runtimes.
	Runtimes []string `json:"runtimes,omitempty"`
}

// ONNXModel represents a single ONNX model file.
type ONNXModel struct {
	Path string `json:"path"`

	// Name is the model name extracted from the path.
	Name string `json:"name"`

	// Size is the file size in bytes.
	Size int64 `json:"size"`

	SizeHuman string `json:"size_human"`

	// Variant indicates the model variant (e.g., "fp32", "fp16", "int8").
	Variant string `json:"variant,omitempty"`

	Optimized bool `json:"optimized,omitempty"`
}

// TransformersInfo contains detailed Transformers model analysis results.
type TransformersInfo struct {
	// Architecture is the model architecture class (e.g., "LlamaForCausalLM", "BertForSequenceClassification").
	Architecture string `json:"architecture"`

	ArchitectureDescription string `json:"architecture_description,omitempty"`

	// Task is the primary task the model is designed for.
	Task string `json:"task,omitempty"`

	TaskDescription string `json:"task_description,omitempty"`

	// ModelType is the model type identifier (e.g., "llama", "bert", "gpt2").
	ModelType string `json:"model_type,omitempty"`

	HiddenSize int `json:"hidden_size,omitempty"`

	NumHiddenLayers int `json:"num_hidden_layers,omitempty"`

	NumAttentionHeads int `json:"num_attention_heads,omitempty"`

	// IntermediateSize is the size of the intermediate (feed-forward) layer.
	IntermediateSize int `json:"intermediate_size,omitempty"`

	VocabSize int `json:"vocab_size,omitempty"`

	MaxPositionEmbeddings int `json:"max_position_embeddings,omitempty"`

	// ContextLength is the effective context window (may differ from max_position_embeddings).
	ContextLength int `json:"context_length,omitempty"`

	// EstimatedParameters is the estimated parameter count string (e.g., "7B", "70B").
	EstimatedParameters string `json:"estimated_parameters,omitempty"`

	// EstimatedParametersNum is the estimated parameter count as a number.
	EstimatedParametersNum int64 `json:"estimated_parameters_num,omitempty"`

	// Precision is the detected model precision (e.g., "fp32", "fp16", "bf16").
	Precision string `json:"precision,omitempty"`

	// IsSharded indicates if the model is split across multiple files.
	IsSharded bool `json:"is_sharded,omitempty"`

	// ShardCount is the number of shards if sharded.
	ShardCount int `json:"shard_count,omitempty"`

	WeightFiles []WeightFile `json:"weight_files,omitempty"`

	Tokenizer *TokenizerInfo `json:"tokenizer,omitempty"`

	// TorchDtype is the torch dtype from config (e.g., "float16", "bfloat16").
	TorchDtype string `json:"torch_dtype,omitempty"`

	// Backends lists compatible inference backends.
	Backends []string `json:"backends,omitempty"`

	SpecialTokens map[string]interface{} `json:"special_tokens,omitempty"`

	GenerationConfig map[string]interface{} `json:"generation_config,omitempty"`
}

// WeightFile represents a model weight file.
type WeightFile struct {
	Path string `json:"path"`

	Name string `json:"name"`

	// Size is the file size in bytes.
	Size int64 `json:"size"`

	SizeHuman string `json:"size_human"`

	// Format is the file format (safetensors, bin, pt).
	Format string `json:"format"`

	// ShardIndex is the shard index (0-based) if sharded.
	ShardIndex int `json:"shard_index,omitempty"`

	ShardTotal int `json:"shard_total,omitempty"`
}

// TokenizerInfo contains tokenizer details.
type TokenizerInfo struct {
	// Type is the tokenizer class (e.g., "LlamaTokenizerFast", "GPT2Tokenizer").
	Type string `json:"type,omitempty"`

	VocabSize int `json:"vocab_size,omitempty"`

	ModelMaxLength int `json:"model_max_length,omitempty"`

	// PaddingSide is the padding side ("left" or "right").
	PaddingSide string `json:"padding_side,omitempty"`

	TruncationSide string `json:"truncation_side,omitempty"`

	// AddBosToken indicates if BOS token is added.
	AddBosToken bool `json:"add_bos_token,omitempty"`

	// AddEosToken indicates if EOS token is added.
	AddEosToken bool `json:"add_eos_token,omitempty"`

	ChatTemplate string `json:"chat_template,omitempty"`

	HasChatTemplate bool `json:"has_chat_template,omitempty"`
}
