// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package sourceproviders

import (
	"context"
	"fmt"

	"github.com/microsoft/azure-linux-dev-tools/internal/app/azldev/core/components"
	"github.com/microsoft/azure-linux-dev-tools/internal/projectconfig"
)

// originHandler is an interface for handling the acquisition of a source file based on its origin type.
// Each origin type (download, cargo-vendor, rust2rpm) has a dedicated handler that encapsulates
// the logic for producing the file at the destination path.
type originHandler interface {
	// Handle acquires or generates the source file described by fileRef, placing the result at destPath.
	// The component is provided for context (e.g., package name resolution).
	// destDirPath is the directory containing all component sources.
	Handle(
		ctx context.Context,
		component components.Component,
		fileRef *projectconfig.SourceFileReference,
		destPath string,
		destDirPath string,
	) error
}

// resolveOriginHandler returns the appropriate [originHandler] for the given origin type.
func (m *sourceManager) resolveOriginHandler(originType projectconfig.OriginType) (originHandler, error) {
	switch originType {
	case projectconfig.OriginTypeURI, "":
		return &downloadOriginHandler{
			dryRunnable:         m.dryRunnable,
			eventListener:       m.eventListener,
			fs:                  m.fs,
			retryConfig:         m.retryConfig,
			lookasideBaseURI:    m.lookasideBaseURI,
			disableOrigins:      m.disableOrigins,
			lookasideDownloader: m.lookasideDownloader,
		}, nil

	case projectconfig.OriginTypeCargoVendor:
		return &cargoVendorOriginHandler{
			cmdFactory:    m.cmdFactory,
			fs:            m.fs,
			dryRunnable:   m.dryRunnable,
			eventListener: m.eventListener,
		}, nil

	case projectconfig.OriginTypeRust2RPM:
		return &rust2rpmOriginHandler{
			cmdFactory:    m.cmdFactory,
			fs:            m.fs,
			dryRunnable:   m.dryRunnable,
			eventListener: m.eventListener,
		}, nil

	default:
		return nil, fmt.Errorf("unsupported origin type %#q", originType)
	}
}

// isGenerativeOrigin returns true if the origin type produces files at runtime rather than
// downloading them. Generative origins always regenerate their output, bypassing the
// "already exists" early-return and lookaside cache checks.
func isGenerativeOrigin(originType projectconfig.OriginType) bool {
	switch originType {
	case projectconfig.OriginTypeCargoVendor, projectconfig.OriginTypeRust2RPM:
		return true
	case projectconfig.OriginTypeURI:
		return false
	default:
		return false
	}
}
