// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package sourceproviders

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"

	"github.com/microsoft/azure-linux-dev-tools/internal/app/azldev/core/components"
	"github.com/microsoft/azure-linux-dev-tools/internal/global/opctx"
	"github.com/microsoft/azure-linux-dev-tools/internal/projectconfig"
	"github.com/microsoft/azure-linux-dev-tools/internal/providers/sourceproviders/fedorasource"
	"github.com/microsoft/azure-linux-dev-tools/internal/utils/downloader"
	"github.com/microsoft/azure-linux-dev-tools/internal/utils/fileutils"
	"github.com/microsoft/azure-linux-dev-tools/internal/utils/retry"
)

// downloadOriginHandler handles source files with [projectconfig.OriginTypeURI] origin.
// It tries the lookaside cache first (when hash info is available), then falls back to
// downloading from the configured URI.
type downloadOriginHandler struct {
	dryRunnable         opctx.DryRunnable
	eventListener       opctx.EventListener
	fs                  opctx.FS
	retryConfig         retry.Config
	lookasideBaseURI    string
	disableOrigins      bool
	lookasideDownloader fedorasource.FedoraSourceDownloader
}

// Ensure [downloadOriginHandler] implements [originHandler].
var _ originHandler = (*downloadOriginHandler)(nil)

// Handle implements [originHandler] for download-type origins.
// It tries the lookaside cache first, then falls back to the configured URI origin.
func (h *downloadOriginHandler) Handle(
	ctx context.Context,
	component components.Component,
	fileRef *projectconfig.SourceFileReference,
	destPath string,
	_ string,
) error {
	httpDownloader, err := downloader.NewHTTPDownloader(h.dryRunnable, h.eventListener, h.fs)
	if err != nil {
		return fmt.Errorf("failed to create HTTP downloader:\n%w", err)
	}

	// Phase 1: Try lookaside cache if hash info is available.
	if fileRef.Hash != "" && fileRef.HashType != "" {
		lookasideErr := h.tryLookasideDownload(ctx, httpDownloader, component, fileRef, destPath)
		if lookasideErr == nil {
			return nil
		}

		slog.Debug("Lookaside cache download failed",
			"filename", fileRef.Filename,
			"error", lookasideErr)
	}

	// Phase 2: Fall back to configured origin (not allowed when disable-origins is set).
	if h.disableOrigins {
		return fmt.Errorf(
			"source file %#q not found in lookaside cache and disable-origins is enabled in the distro config",
			fileRef.Filename,
		)
	}

	if fileRef.Origin.Type == "" && fileRef.Origin.Uri == "" {
		return fmt.Errorf("source file %#q not found in lookaside cache and no origin configured",
			fileRef.Filename)
	}

	return h.downloadFromURI(ctx, httpDownloader, fileRef, destPath)
}

// tryLookasideDownload attempts to download a source file from the lookaside cache.
func (h *downloadOriginHandler) tryLookasideDownload(
	ctx context.Context,
	httpDownloader downloader.Downloader,
	component components.Component,
	fileRef *projectconfig.SourceFileReference,
	destPath string,
) error {
	if h.lookasideBaseURI == "" {
		return errors.New("no lookaside cache configured")
	}

	packageName := resolvePackageName(component)

	sourceURL, err := fedorasource.BuildLookasideURL(
		h.lookasideBaseURI, packageName, fileRef.Filename,
		strings.ToUpper(string(fileRef.HashType)), fileRef.Hash,
	)
	if err != nil {
		return fmt.Errorf("failed to build lookaside URL for %#q:\n%w", fileRef.Filename, err)
	}

	slog.Info("Downloading source file from lookaside cache...",
		"filename", fileRef.Filename,
		"url", sourceURL)

	err = h.downloadAndValidate(ctx, httpDownloader, sourceURL, destPath, fileRef)
	if err != nil {
		return fmt.Errorf("lookaside cache download failed for %#q:\n%w", fileRef.Filename, err)
	}

	return nil
}

// downloadFromURI downloads a source file from its configured URI.
func (h *downloadOriginHandler) downloadFromURI(
	ctx context.Context,
	httpDownloader downloader.Downloader,
	fileRef *projectconfig.SourceFileReference,
	destPath string,
) error {
	if fileRef.Origin.Uri == "" {
		return fmt.Errorf("no URI configured for source file %#q with origin type %#q",
			fileRef.Filename, fileRef.Origin.Type)
	}

	slog.Info("Downloading source file from origin URL...",
		"filename", fileRef.Filename,
		"origin", fileRef.Origin.Uri,
		"destination", destPath)

	err := h.downloadAndValidate(ctx, httpDownloader, fileRef.Origin.Uri, destPath, fileRef)
	if err != nil {
		return fmt.Errorf("failed to retrieve source file %#q:\n%w", fileRef.Filename, err)
	}

	return nil
}

// downloadAndValidate downloads a file from the given URL with retries, optionally
// validating its hash. On failure, any partial file is cleaned up.
func (h *downloadOriginHandler) downloadAndValidate(
	ctx context.Context,
	httpDownloader downloader.Downloader,
	sourceURL string,
	destPath string,
	fileRef *projectconfig.SourceFileReference,
) error {
	err := retry.Do(ctx, h.retryConfig, func() error {
		_ = h.fs.Remove(destPath)

		downloadErr := httpDownloader.Download(ctx, sourceURL, destPath)
		if downloadErr != nil {
			return fmt.Errorf("failed to download %#q from %#q:\n%w",
				fileRef.Filename, sourceURL, downloadErr)
		}

		if fileRef.Hash != "" && fileRef.HashType != "" {
			hashErr := fileutils.ValidateFileHash(h.dryRunnable, h.fs, fileRef.HashType, destPath, fileRef.Hash)
			if hashErr != nil {
				return fmt.Errorf("hash validation failed for %#q:\n%w", fileRef.Filename, hashErr)
			}
		}

		return nil
	})
	if err != nil {
		_ = h.fs.Remove(destPath)

		return fmt.Errorf("download failed:\n%w", err)
	}

	return nil
}
