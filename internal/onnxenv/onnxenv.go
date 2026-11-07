// Package onnxenv holds the one piece of global, cross-package ONNX runtime
// state this program has to share: onnxruntime_go's process-wide environment,
// which its underlying C API forbids initializing more than once per
// process. Both internal/facerec (SCRFD/ArcFace) and internal/voicerec
// (speaker embedding) load their own model sessions independently, but both
// must funnel their one-time environment setup through Ensure here --
// otherwise enabling both facial and voice recognition in the same run would
// attempt to initialize the environment twice and fail the second one.
package onnxenv

import (
	"fmt"
	"sync"

	ort "github.com/yalue/onnxruntime_go"
)

var (
	once sync.Once
	err  error
)

// Ensure initializes the onnxruntime_go environment exactly once per
// process, regardless of how many times or from how many packages it's
// called. libPath, if non-empty, is set as the shared library path before
// that first initialization; a later call (from the same or a different
// package) with a different libPath is silently ignored, since every caller
// in this program is only ever configured with one -onnxruntime-lib value
// per process (see run.go).
func Ensure(libPath string) error {
	once.Do(func() {
		if libPath != "" {
			ort.SetSharedLibraryPath(libPath)
		}
		err = ort.InitializeEnvironment()
	})
	return err
}

// NewSessionOptions returns a fresh *ort.SessionOptions configured to run on
// provider ("" and "cpu" are equivalent, and the default -- plain CPU
// execution; "coreml" additionally appends onnxruntime's CoreML execution
// provider). Every ONNX-backed feature in this codebase (internal/silerovad,
// internal/facerec, internal/voicerec, internal/textembed) funnels its
// session construction through this one function rather than each
// duplicating its own provider-selection logic, mirroring how they already
// all funnel one-time environment setup through Ensure above -- Ensure must
// be called first (this returns an error otherwise, since NewSessionOptions
// itself requires an initialized environment).
//
// Unlike the sherpa-onnx-go-backed backends (internal/sherpazipformer/
// internal/sherpawhisper/internal/sherpatts), which each expose sherpa-onnx's
// own "cpu"/"cuda"/"coreml" Provider string more or less directly, this
// codebase's own onnxruntime_go dependency requires an explicit API call per
// provider rather than a single string field, so "coreml" is the only
// non-cpu option offered here -- CUDA would need its own CUDAProviderOptions
// construction this codebase has no use for on Linux/macOS-only hardware,
// and isn't part of what any caller has asked for.
//
// CoreML is opt-in and unverified from Linux, deliberately: real-world
// reports (see e.g. k2-fsa/sherpa-onnx#2910) show it isn't a guaranteed win
// even on Apple Silicon -- CoreML partitions a model graph and falls back to
// CPU for any op it doesn't support, and for some model architectures the
// partitioning/data-copy overhead outweighs whatever the Neural Engine/GPU
// would have saved. Each caller's own -*-provider flag lets a macOS user A/B
// test CoreML against the "cpu" default per model and keep whichever wins,
// rather than this codebase guessing at hardware it was never run on.
// AppendExecutionProviderCoreMLV2 (the current, non-deprecated API) is used
// with "MLComputeUnits": "ALL", letting CoreML itself pick among the CPU/
// GPU/Neural Engine rather than pinning one -- returns a clear error (naming
// the provider) if the loaded onnxruntime build lacks CoreML support at all
// (e.g. running this same code path on Linux), rather than a confusing
// native failure deeper in session construction.
func NewSessionOptions(provider string) (*ort.SessionOptions, error) {
	// Validated before ever touching onnxruntime (which requires Ensure to
	// have already succeeded) so a bad provider name fails fast and
	// hermetically-testably, independent of whether a real onnxruntime
	// environment is available at all.
	if provider != "" && provider != "cpu" && provider != "coreml" {
		return nil, fmt.Errorf(`unknown onnxruntime execution provider %q, want "cpu" or "coreml"`, provider)
	}

	opts, err := ort.NewSessionOptions()
	if err != nil {
		return nil, fmt.Errorf("create onnxruntime session options: %w", err)
	}
	if provider == "coreml" {
		if err := opts.AppendExecutionProviderCoreMLV2(map[string]string{
			"MLComputeUnits": "ALL",
		}); err != nil {
			opts.Destroy()
			return nil, fmt.Errorf("append CoreML execution provider (only supported on macOS, and only if this onnxruntime build includes it): %w", err)
		}
	}
	// "" and "cpu" need no provider appended -- onnxruntime already falls
	// back to its built-in CPU execution provider when none is registered.
	return opts, nil
}
