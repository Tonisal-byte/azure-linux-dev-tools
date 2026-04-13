// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package synthesis

import (
	"fmt"
	"log/slog"
	"path/filepath"
	"strings"

	"github.com/microsoft/azure-linux-dev-tools/internal/app/azldev/core/components"
	"github.com/microsoft/azure-linux-dev-tools/internal/projectconfig"
)

// RustVendorSynthesizer generates synthetic [projectconfig.SourceFileReference] entries
// for Rust vendor bundling. When rust-vendor is enabled on a component, it produces
// two entries:
//   - A rust2rpm origin entry that regenerates the spec file with vendor support.
//   - A cargo-vendor origin entry that generates the vendored dependencies tarball.
//
// The .crates file is either taken from the component's explicit configuration or
// auto-discovered by globbing *.crates in the sources directory.
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

	// Derive the vendor tarball name from the crates file name.
	// Convention: <crate-name>-<version>-vendor.tar.xz where the crate name
	// and version are derived from the .crates file (e.g., "foo-1.2.3.crates" → "foo-1.2.3").
	baseName := strings.TrimSuffix(cratesFile, filepath.Ext(cratesFile))
	vendorTarball := baseName + "-vendor.tar.xz"

	return []projectconfig.SourceFileReference{
		{
			// rust2rpm regenerates the spec with vendor support.
			Filename: componentName + ".spec",
			Origin: projectconfig.Origin{
				Type:       projectconfig.OriginTypeRust2RPM,
				CratesFile: cratesFile,
			},
		},
		{
			// cargo vendor generates the vendored dependencies tarball.
			Filename: vendorTarball,
			Origin: projectconfig.Origin{
				Type:       projectconfig.OriginTypeCargoVendor,
				CratesFile: cratesFile,
			},
		},
	}, nil
}

// resolveCratesFile determines the .crates file to use for Rust vendor bundling.
// If an explicit name is provided, it is returned directly. Otherwise, the function
// globs *.crates in the sources directory and returns an error if zero or multiple
// matches are found.
func resolveCratesFile(explicitName string, sourcesDir string) (string, error) {
	if explicitName != "" {
		return explicitName, nil
	}

	// Auto-discover by globbing *.crates in the sources directory.
	pattern := filepath.Join(sourcesDir, "*.crates")

	matches, err := filepath.Glob(pattern)
	if err != nil {
		return "", fmt.Errorf("failed to glob for .crates files in %#q:\n%w", sourcesDir, err)
	}

	switch len(matches) {
	case 0:
		return "", fmt.Errorf(
			"no .crates file found in %#q; either place a .crates file in the sources "+
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
			"multiple .crates files found in %#q: %v; set crates-file in the "+
				"rust-vendor configuration to disambiguate",
			sourcesDir, names,
		)
	}
}
