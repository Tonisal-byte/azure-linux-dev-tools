// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package sourceproviders

import (
	"context"
	"fmt"
	"log/slog"
	"os/exec"
	"path/filepath"

	"github.com/microsoft/azure-linux-dev-tools/internal/app/azldev/core/components"
	"github.com/microsoft/azure-linux-dev-tools/internal/global/opctx"
	"github.com/microsoft/azure-linux-dev-tools/internal/projectconfig"
	"github.com/microsoft/azure-linux-dev-tools/internal/utils/fileutils"
)

// cargoVendorOriginHandler handles source files with [projectconfig.OriginTypeCargoVendor] origin.
// It runs `cargo vendor` against a .crates file to produce a vendored dependencies tarball.
type cargoVendorOriginHandler struct {
	cmdFactory    opctx.CmdFactory
	fs            opctx.FS
	dryRunnable   opctx.DryRunnable
	eventListener opctx.EventListener
}

// Ensure [cargoVendorOriginHandler] implements [originHandler].
var _ originHandler = (*cargoVendorOriginHandler)(nil)

// Handle implements [originHandler] for cargo-vendor origins.
// It generates a vendor tarball by running cargo vendor in the sources directory,
// using the .crates file specified in the origin configuration.
func (h *cargoVendorOriginHandler) Handle(
	ctx context.Context,
	component components.Component,
	fileRef *projectconfig.SourceFileReference,
	destPath string,
	destDirPath string,
) error {
	cratesFile := fileRef.Origin.CratesFile
	if cratesFile == "" {
		return fmt.Errorf("no crates file specified for cargo-vendor origin of source file %#q",
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

	slog.Info("Generating vendor tarball via cargo vendor...",
		"component", component.GetName(),
		"cratesFile", cratesFile,
		"output", fileRef.Filename)

	// Run cargo vendor to create the vendor directory.
	vendorDir := filepath.Join(destDirPath, "vendor")

	execCmd := exec.CommandContext(ctx, "cargo", "vendor", "--manifest-path", cratesFilePath, vendorDir)
	execCmd.Dir = destDirPath

	cargoCmd, err := h.cmdFactory.Command(execCmd)
	if err != nil {
		return fmt.Errorf("failed to create cargo vendor command:\n%w", err)
	}

	cargoCmd.SetDescription("Generating vendored Rust dependencies")

	err = cargoCmd.Run(ctx)
	if err != nil {
		return fmt.Errorf("cargo vendor failed for component %#q:\n%w", component.GetName(), err)
	}

	// Package the vendor directory into a tarball.
	tarCmd, err := h.cmdFactory.Command(
		exec.CommandContext(ctx, "tar", "cJf", destPath, "-C", destDirPath, "vendor"),
	)
	if err != nil {
		return fmt.Errorf("failed to create tar command:\n%w", err)
	}

	tarCmd.SetDescription("Packaging vendor tarball")

	err = tarCmd.Run(ctx)
	if err != nil {
		return fmt.Errorf("failed to create vendor tarball for component %#q:\n%w", component.GetName(), err)
	}

	// Clean up the vendor directory; we only need the tarball.
	_ = h.fs.RemoveAll(vendorDir)

	slog.Info("Successfully generated vendor tarball",
		"component", component.GetName(),
		"output", fileRef.Filename)

	return nil
}
