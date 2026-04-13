// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package synthesis

import (
	"github.com/microsoft/azure-linux-dev-tools/internal/app/azldev/core/components"
	"github.com/microsoft/azure-linux-dev-tools/internal/projectconfig"
)

// Synthesizer generates synthetic [projectconfig.SourceFileReference] entries for a component.
// Each implementation handles a specific kind of source generation (e.g., Rust vendor bundling,
// Go module vendoring). Returning an empty slice signals that the synthesizer has nothing
// to contribute for the given component.
type Synthesizer interface {
	// SynthesizeSourceFiles inspects the component's configuration and, when applicable,
	// returns source-file references that should be prepended to the component's SourceFiles
	// for processing by FetchFiles.
	// sourcesDir is the directory containing the component's fetched sources.
	SynthesizeSourceFiles(
		component components.Component,
		sourcesDir string,
	) ([]projectconfig.SourceFileReference, error)
}

// DefaultSynthesizers returns the standard set of synthesizers that should be
// registered with a [sources.SourcePreparer]. New language-specific synthesizers
// should be added here so that all callers pick them up automatically.
func DefaultSynthesizers() []Synthesizer {
	return []Synthesizer{
		&RustVendorSynthesizer{},
	}
}
