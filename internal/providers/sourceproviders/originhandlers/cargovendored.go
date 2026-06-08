// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package originhandlers

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	"github.com/microsoft/azure-linux-dev-tools/internal/global/opctx"
	"github.com/microsoft/azure-linux-dev-tools/internal/projectconfig"
	"github.com/microsoft/azure-linux-dev-tools/internal/utils/archive"
)

// NewCargoVendoredHandler creates a [Handler] that generates a vendor tarball
// by extracting the source archive named in [projectconfig.Origin.Source],
// running 'cargo vendor' against the 'Cargo.toml' found inside it, and
// archiving the result. The tarball contains a top-level 'vendor/' directory,
// matching the Fedora/Azure Linux convention consumed by '%cargo_prep -v vendor'.
//
// The archive compression format is inferred from the source file's
// [projectconfig.SourceFileReference.Filename] extension.
func NewCargoVendoredHandler(cmdFactory opctx.CmdFactory) Handler {
	return func(
		ctx context.Context,
		fileRef *projectconfig.SourceFileReference,
		destPath string,
	) error {
		return generateCargoVendorArchive(ctx, cmdFactory, fileRef, destPath)
	}
}

// generateCargoVendorArchive extracts the source archive specified by
// [projectconfig.Origin.Source], locates the 'Cargo.toml' inside it,
// runs 'cargo vendor', and archives the resulting 'vendor/' tree.
func generateCargoVendorArchive(
	ctx context.Context,
	cmdFactory opctx.CmdFactory,
	fileRef *projectconfig.SourceFileReference,
	destPath string,
) error {
	if !cmdFactory.CommandInSearchPath("cargo") {
		return errors.New(
			"'cargo' is not available in PATH; " +
				"it is required for origin type 'cargo-vendored'")
	}

	sourceDir := filepath.Dir(destPath)

	slog.Info("Generating cargo vendor archive...",
		"filename", fileRef.Filename,
		"source", fileRef.Origin.Source,
		"destination", destPath)

	outComp, err := archive.DetectCompression(fileRef.Filename)
	if err != nil {
		return fmt.Errorf("failed to detect compression for %#q:\n%w",
			fileRef.Filename, err)
	}

	tmpDir, err := os.MkdirTemp("", "cargo-vendor-*")
	if err != nil {
		return fmt.Errorf(
			"failed to create temporary directory for cargo vendor:\n%w", err)
	}

	defer os.RemoveAll(tmpDir)

	manifestPath, err := extractAndPrepareSource(ctx, cmdFactory, fileRef, sourceDir, tmpDir)
	if err != nil {
		return err
	}

	// Run cargo vendor into a separate subtree so the archive contains
	// only the vendor/ directory at the top level.
	vendorBase := filepath.Join(tmpDir, "vendored")
	vendorDir := filepath.Join(vendorBase, "vendor")

	if err := runCargoVendor(ctx, cmdFactory, manifestPath, vendorDir); err != nil {
		return fmt.Errorf("'cargo vendor' failed for %#q:\n%w",
			fileRef.Filename, err)
	}

	if err := archive.CreateDeterministicArchive(destPath, vendorBase, outComp); err != nil {
		return fmt.Errorf("failed to create vendor archive %#q:\n%w",
			fileRef.Filename, err)
	}

	slog.Info("Successfully generated cargo vendor archive",
		"filename", fileRef.Filename,
		"destination", destPath)

	return nil
}

// extractAndPrepareSource extracts the source archive, applies any pre-handler
// patches, and returns the path to the 'Cargo.toml' manifest.
func extractAndPrepareSource(
	ctx context.Context,
	cmdFactory opctx.CmdFactory,
	fileRef *projectconfig.SourceFileReference,
	sourceDir string,
	tmpDir string,
) (string, error) {
	sourceArchivePath := filepath.Join(sourceDir, fileRef.Origin.Source)
	extractDir := filepath.Join(tmpDir, "source")

	srcComp, err := detectSourceCompression(fileRef.Origin.Source)
	if err != nil {
		return "", fmt.Errorf(
			"failed to detect compression for source archive %#q:\n%w",
			fileRef.Origin.Source, err)
	}

	if err := archive.Extract(sourceArchivePath, extractDir, srcComp); err != nil {
		return "", fmt.Errorf(
			"failed to extract source archive %#q:\n%w",
			fileRef.Origin.Source, err)
	}

	// If the archive has a single top-level directory (the standard tarball
	// convention), use it as the working directory for patches and manifest
	// lookup — matching RPM's %setup / %patch behavior.
	sourceRoot, err := findSourceRoot(extractDir)
	if err != nil {
		return "", fmt.Errorf(
			"failed to determine source root in %#q:\n%w",
			fileRef.Origin.Source, err)
	}

	if err := applyOriginPatches(ctx, cmdFactory, fileRef.Origin.Patches, sourceDir, sourceRoot); err != nil {
		return "", fmt.Errorf(
			"failed to apply pre-vendor patches for %#q:\n%w",
			fileRef.Filename, err)
	}

	manifestPath, err := findCargoManifest(sourceRoot)
	if err != nil {
		return "", fmt.Errorf(
			"failed to locate 'Cargo.toml' in source archive %#q:\n%w",
			fileRef.Origin.Source, err)
	}

	return manifestPath, nil
}

// runCargoVendor executes 'cargo vendor --manifest-path <manifestPath> <vendorDir>'.
func runCargoVendor(
	ctx context.Context,
	cmdFactory opctx.CmdFactory,
	manifestPath string,
	vendorDir string,
) error {
	var stderr strings.Builder

	rawCmd := exec.CommandContext(ctx, "cargo", "vendor",
		"--manifest-path", manifestPath,
		vendorDir,
	)
	rawCmd.Stderr = &stderr

	cmd, err := cmdFactory.Command(rawCmd)
	if err != nil {
		return fmt.Errorf("failed to create 'cargo vendor' command:\n%w", err)
	}

	cmd.SetDescription("Generating vendor tarball via cargo vendor")

	_, err = cmd.RunAndGetOutput(ctx)
	if err != nil {
		return fmt.Errorf("command failed:\n%w\nstderr: %s", err, stderr.String())
	}

	return nil
}

// applyOriginPatches applies the listed patch files to the extracted source tree.
// Each patch is applied with 'patch -p1' from the extract directory.
func applyOriginPatches(
	ctx context.Context,
	cmdFactory opctx.CmdFactory,
	patches []string,
	sourceDir string,
	extractDir string,
) error {
	if len(patches) == 0 {
		return nil
	}

	if !cmdFactory.CommandInSearchPath("patch") {
		return errors.New(
			"'patch' is not available in PATH; " +
				"it is required when 'origin.patches' is specified")
	}

	for _, patchFile := range patches {
		patchPath, err := filepath.Abs(filepath.Join(sourceDir, patchFile))
		if err != nil {
			return fmt.Errorf("failed to resolve absolute path for patch %#q:\n%w", patchFile, err)
		}

		slog.Info("Applying pre-vendor patch", "patch", patchFile)

		var stderr strings.Builder

		rawCmd := exec.CommandContext(ctx, "patch", "-p1", "-i", patchPath)
		rawCmd.Dir = extractDir
		rawCmd.Stderr = &stderr

		cmd, err := cmdFactory.Command(rawCmd)
		if err != nil {
			return fmt.Errorf("failed to create 'patch' command for %#q:\n%w", patchFile, err)
		}

		cmd.SetDescription(fmt.Sprintf("Applying patch %#q before cargo vendor", patchFile))

		if _, err := cmd.RunAndGetOutput(ctx); err != nil {
			return fmt.Errorf("failed to apply patch %#q:\n%w\nstderr: %s", patchFile, err, stderr.String())
		}
	}

	return nil
}

// detectSourceCompression determines the compression type for a source archive
// filename. In addition to the formats supported by [archive.DetectCompression],
// it recognises '.crate' files as gzip-compressed tar archives.
func detectSourceCompression(filename string) (archive.Compression, error) {
	if strings.HasSuffix(strings.ToLower(filename), ".crate") {
		return archive.CompressionGzip, nil
	}

	comp, err := archive.DetectCompression(filename)
	if err != nil {
		return archive.CompressionNone, fmt.Errorf(
			"detecting compression for %#q:\n%w", filename, err)
	}

	return comp, nil
}

// findSourceRoot returns the directory to use as the working directory for
// patches and manifest lookup. If the extraction produced a single top-level
// directory (the standard source tarball convention), that directory is returned.
// Otherwise, extractDir itself is returned.
func findSourceRoot(extractDir string) (string, error) {
	entries, err := os.ReadDir(extractDir)
	if err != nil {
		return "", fmt.Errorf("reading extract directory:\n%w", err)
	}

	if len(entries) == 1 && entries[0].IsDir() {
		return filepath.Join(extractDir, entries[0].Name()), nil
	}

	return extractDir, nil
}

// findCargoManifest walks the extracted source tree and returns the path to the
// first 'Cargo.toml' found. It prefers a 'Cargo.toml' at the shallowest depth.
func findCargoManifest(root string) (string, error) {
	var best string

	bestDepth := -1

	err := filepath.WalkDir(root, func(path string, entry os.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}

		if entry.IsDir() {
			return nil
		}

		if entry.Name() != "Cargo.toml" {
			return nil
		}

		rel, relErr := filepath.Rel(root, path)
		if relErr != nil {
			return fmt.Errorf("computing relative path for %#q:\n%w", path, relErr)
		}

		depth := strings.Count(rel, string(filepath.Separator))

		if bestDepth < 0 || depth < bestDepth {
			best = path
			bestDepth = depth
		}

		return nil
	})
	if err != nil {
		return "", fmt.Errorf("walking extracted source tree:\n%w", err)
	}

	if best == "" {
		return "", errors.New("no 'Cargo.toml' found in extracted source archive")
	}

	return best, nil
}
