// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package originhandlers

import (
	"context"
	"fmt"
	"log/slog"

	"github.com/microsoft/azure-linux-dev-tools/internal/projectconfig"
)

// DownloadFunc downloads a file from sourceURL to destPath with retry and
// hash validation. The source manager provides this as a closure that
// captures the HTTP downloader, retry config, and filesystem.
type DownloadFunc func(
	ctx context.Context,
	sourceURL string,
	destPath string,
	fileRef *projectconfig.SourceFileReference,
) error

// NewURIHandler creates a [Handler] that downloads a source file from its
// configured [projectconfig.Origin.Uri].
func NewURIHandler(download DownloadFunc) Handler {
	return func(
		ctx context.Context,
		fileRef *projectconfig.SourceFileReference,
		destPath string,
	) error {
		if fileRef.Origin.Uri == "" {
			return fmt.Errorf(
				"no URI configured for source file %#q with origin type %#q",
				fileRef.Filename, fileRef.Origin.Type)
		}

		slog.Info("Downloading source file from origin URL...",
			"filename", fileRef.Filename,
			"origin", fileRef.Origin.Uri,
			"destination", destPath)

		err := download(ctx, fileRef.Origin.Uri, destPath, fileRef)
		if err != nil {
			return fmt.Errorf("failed to retrieve source file %#q:\n%w",
				fileRef.Filename, err)
		}

		return nil
	}
}
