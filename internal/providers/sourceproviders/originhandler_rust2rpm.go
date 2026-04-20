// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package sourceproviders

import (
	"bytes"
	"context"
	"fmt"
	"log/slog"
	"os/exec"
	"path/filepath"
	"time"

	"github.com/microsoft/azure-linux-dev-tools/internal/app/azldev/core/components"
	"github.com/microsoft/azure-linux-dev-tools/internal/global/opctx"
	"github.com/microsoft/azure-linux-dev-tools/internal/projectconfig"
	"github.com/microsoft/azure-linux-dev-tools/internal/utils/fileutils"
	"github.com/microsoft/azure-linux-dev-tools/internal/utils/tarutil"
)

// rust2rpmAutoMode is the mode argument passed to rust2rpm for automatic
// spec generation.
const rust2rpmAutoMode = "auto"

// rust2rpmOriginHandler handles source files with [projectconfig.OriginTypeRust2RPM] origin.
// It runs `rust2rpm -V auto` against a .crate file to produce a vendor-aware spec file.
// If a rust2rpm.toml configuration file is present in the sources directory, it is
// passed via the '-C' flag. The crate is identified via '-O name@version'.
type rust2rpmOriginHandler struct {
	cmdFactory    opctx.CmdFactory
	fs            opctx.FS
	dryRunnable   opctx.DryRunnable
	eventListener opctx.EventListener
}

// Ensure [rust2rpmOriginHandler] implements [originHandler].
var _ originHandler = (*rust2rpmOriginHandler)(nil)

// Handle implements [originHandler] for rust2rpm origins.
// It generates a vendor-aware spec by running rust2rpm -V auto with the specified .crate file,
// overwriting the existing spec at destPath.
func (h *rust2rpmOriginHandler) Handle(
	ctx context.Context,
	component components.Component,
	fileRef *projectconfig.SourceFileReference,
	destPath string,
	destDirPath string,
) error {
	cratesFile := fileRef.Origin.CratesFile
	if cratesFile == "" {
		return fmt.Errorf("no crates file specified for rust2rpm origin of source file %#q",
			fileRef.Filename)
	}

	cratesFilePath := filepath.Join(destDirPath, cratesFile)

	cratesExists, err := fileutils.Exists(h.fs, cratesFilePath)
	if err != nil {
		return fmt.Errorf("failed to check existence of crates file %#q:\n%w", cratesFilePath, err)
	}

	if !cratesExists {
		return fmt.Errorf("crates file %#q not found in sources directory %#q",
			cratesFile, destDirPath)
	}

	slog.Info("Generating vendor-aware spec via rust2rpm...",
		"component", component.GetName(),
		"cratesFile", cratesFile,
		"output", fileRef.Filename)

	// Build the rust2rpm command: rust2rpm -a -V auto --path <crate> [-C rust2rpm.toml] -o <dir>
	args, err := h.buildRust2RPMArgs(destDirPath, component, cratesFile)
	if err != nil {
		return err
	}

	// Run rust2rpm in the sources directory.
	// rust2rpm generates the spec in the current working directory.
	var stderr bytes.Buffer

	execCmd := exec.CommandContext(ctx, "rust2rpm", args...)
	execCmd.Dir = destDirPath
	execCmd.Stderr = &stderr

	rust2rpmCmd, err := h.cmdFactory.Command(execCmd)
	if err != nil {
		return fmt.Errorf("failed to create rust2rpm command:\n%w", err)
	}

	rust2rpmCmd.SetDescription("Generating vendor-aware RPM spec via rust2rpm")

	err = rust2rpmCmd.Run(ctx)
	if err != nil {
		return fmt.Errorf("rust2rpm failed for component %#q:\n%s\n%w",
			component.GetName(), stderr.String(), err)
	}

	// Verify the spec was generated at the expected path.
	specExists, err := fileutils.Exists(h.fs, destPath)
	if err != nil {
		return fmt.Errorf("failed to check existence of generated spec %#q:\n%w", destPath, err)
	}

	if !specExists {
		return fmt.Errorf("rust2rpm did not produce expected spec file %#q for component %#q",
			filepath.Base(destPath), component.GetName())
	}

	// Normalize any generated vendor tarballs for reproducibility.
	if err := h.normalizeVendorTarballs(component, destDirPath); err != nil {
		return err
	}

	slog.Info("Successfully generated vendor-aware spec",
		"component", component.GetName(),
		"output", fileRef.Filename)

	return nil
}

func (h *rust2rpmOriginHandler) buildRust2RPMArgs(
	destDirPath string,
	component components.Component,
	crateFile string,
) ([]string, error) {
	// Ensure destDirPath is absolute to avoid path resolution issues.
	absDestDirPath, err := filepath.Abs(destDirPath)
	if err != nil {
		return nil, fmt.Errorf("failed to get absolute path for %#q:\n%w", destDirPath, err)
	}

	// Use --path to point rust2rpm at the local .crate file directly,
	// avoiding any crates.io lookups.
	crateFilePath := filepath.Join(absDestDirPath, crateFile)
	args := []string{"-a", "-V", rust2rpmAutoMode, "-r", "--path", crateFilePath}

	rust2rpmTomlPath := filepath.Join(absDestDirPath, "rust2rpm.toml")

	tomlExists, err := fileutils.Exists(h.fs, rust2rpmTomlPath)
	if err != nil {
		return nil, fmt.Errorf("failed to check existence of rust2rpm.toml in %#q:\n%w", absDestDirPath, err)
	}

	if tomlExists {
		slog.Info("Found rust2rpm.toml, including in command",
			"component", component.GetName())

		// Pass the upstream rust2rpm.toml straight through. If its
		// '[extra-sources]' entries collide with the Source slots
		// rust2rpm reserves under '--vendor auto' (Source1 = vendor
		// tarball, Source2 = vendor license metadata), the right fix
		// is to renumber those entries in the toml itself (e.g. set
		// 'number = 3' on the colliding extra-source and update any
		// '%{SOURCEn}' references in the toml's '[scripts]' section).
		// We intentionally do not mutate the toml at runtime because
		// the surrounding spec context (script references, patch
		// numbering, etc.) needs to be re-pointed in lockstep, which
		// the upstream maintainer is best positioned to do.
		args = append(args, "-C", rust2rpmTomlPath)
	}

	args = append(args, "-o", absDestDirPath)

	return args, nil
}

// normalizeVendorTarballs finds and normalizes vendor tarballs produced by rust2rpm
// for reproducibility. Only targets '*-vendor.tar.*' files to avoid modifying upstream
// source tarballs where original file permissions may be meaningful to the build.
func (h *rust2rpmOriginHandler) normalizeVendorTarballs(
	component components.Component,
	dirPath string,
) error {
	pattern := filepath.Join(dirPath, "*-vendor.tar.gz")

	matches, err := fileutils.Glob(h.fs, pattern)
	if err != nil {
		return fmt.Errorf("failed to glob for tarballs in %#q:\n%w", dirPath, err)
	}

	timestamp := time.Unix(0, 0).UTC()

	for _, match := range matches {
		slog.Info("Normalizing vendor tarball for reproducibility",
			"component", component.GetName(),
			"path", filepath.Base(match))

		if err := tarutil.NormalizeTarGz(h.fs, match, timestamp); err != nil {
			return fmt.Errorf("failed to normalize tarball %#q:\n%w", match, err)
		}
	}

	return nil
}
