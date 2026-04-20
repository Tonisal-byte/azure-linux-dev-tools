// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package synthesis

import (
	"fmt"
	"log/slog"
	"path/filepath"

	"github.com/microsoft/azure-linux-dev-tools/internal/app/azldev/core/components"
	"github.com/microsoft/azure-linux-dev-tools/internal/projectconfig"
)

// RustVendorSynthesizer generates a synthetic [projectconfig.SourceFileReference] entry
// for Rust vendor bundling. When rust-vendor is enabled on a component, it produces
// a rust2rpm origin entry that regenerates the spec file with vendor support.
// The rust2rpm tool also generates the vendored dependencies tarball automatically.
//
// The .crate file is either taken from the component's explicit configuration or
// auto-discovered by globbing *.crate in the sources directory.
type RustVendorSynthesizer struct{}

// Ensure [RustVendorSynthesizer] implements [Synthesizer].
var _ Synthesizer = (*RustVendorSynthesizer)(nil)

// SynthesizeSourceFiles implements [Synthesizer] for Rust vendor bundling.
// Returns an empty slice when rust-vendor is not enabled.
func (s *RustVendorSynthesizer) SynthesizeSourceFiles(
	component components.Component,
	sourcesDir string,
) ([]projectconfig.SourceFileReference, error) {
	cfg := component.GetConfig().RustVendor
	if !cfg.Enabled {
		return nil, nil
	}

	cratesFile, err := resolveCratesFile(cfg.CratesFile, sourcesDir)
	if err != nil {
		return nil, fmt.Errorf("failed to resolve crates file for component %#q:\n%w",
			component.GetName(), err)
	}

	slog.Info("Rust vendor bundling enabled",
		"component", component.GetName(),
		"cratesFile", cratesFile)

	componentName := component.GetName()

	return []projectconfig.SourceFileReference{
		{
			// rust2rpm -V generates the vendor-aware spec and vendor tarball.
			Filename: componentName + ".spec",
			Origin: projectconfig.Origin{
				Type:       projectconfig.OriginTypeRust2RPM,
				CratesFile: cratesFile,
			},
		},
	}, nil
}

// resolveCratesFile determines the .crate file to use for Rust vendor bundling.
// If an explicit name is provided, it is returned directly. Otherwise, the function
// globs *.crate in the sources directory and returns an error if zero or multiple
// matches are found.
func resolveCratesFile(explicitName string, sourcesDir string) (string, error) {
	if explicitName != "" {
		return explicitName, nil
	}

	// Auto-discover by globbing *.crate in the sources directory.
	pattern := filepath.Join(sourcesDir, "*.crate")

	matches, err := filepath.Glob(pattern)
	if err != nil {
		return "", fmt.Errorf("failed to glob for .crate files in %#q:\n%w", sourcesDir, err)
	}

	switch len(matches) {
	case 0:
		return "", fmt.Errorf(
			"no .crate file found in %#q; either place a .crate file in the sources "+
				"directory or set crates-file in the rust-vendor configuration",
			sourcesDir,
		)

	case 1:
		return filepath.Base(matches[0]), nil

	default:
		names := make([]string, len(matches))
		for i, m := range matches {
			names[i] = filepath.Base(m)
		}

		return "", fmt.Errorf(
			"multiple .crate files found in %#q: %v; set crates-file in the "+
				"rust-vendor configuration to disambiguate",
			sourcesDir, names,
		)
	}
}
