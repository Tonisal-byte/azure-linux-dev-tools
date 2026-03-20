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

// rust2rpmOriginHandler handles source files with [projectconfig.OriginTypeRust2RPM] origin.
// It runs `rust2rpm --vendor` against a .crates file to produce a vendor-aware spec file.
type rust2rpmOriginHandler struct {
	cmdFactory    opctx.CmdFactory
	fs            opctx.FS
	dryRunnable   opctx.DryRunnable
	eventListener opctx.EventListener
}

// Ensure [rust2rpmOriginHandler] implements [originHandler].
var _ originHandler = (*rust2rpmOriginHandler)(nil)

// Handle implements [originHandler] for rust2rpm origins.
// It generates a vendor-aware spec by running rust2rpm --vendor with the specified .crates file,
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

	// Run rust2rpm --vendor <crates-file> in the sources directory.
	// rust2rpm generates the spec in the current working directory.
	execCmd := exec.CommandContext(ctx, "rust2rpm", "--vendor", cratesFile)
	execCmd.Dir = destDirPath

	rust2rpmCmd, err := h.cmdFactory.Command(execCmd)
	if err != nil {
		return fmt.Errorf("failed to create rust2rpm command:\n%w", err)
	}

	rust2rpmCmd.SetDescription("Generating vendor-aware RPM spec via rust2rpm")

	err = rust2rpmCmd.Run(ctx)
	if err != nil {
		return fmt.Errorf("rust2rpm failed for component %#q:\n%w", component.GetName(), err)
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

	slog.Info("Successfully generated vendor-aware spec",
		"component", component.GetName(),
		"output", fileRef.Filename)

	return nil
}
