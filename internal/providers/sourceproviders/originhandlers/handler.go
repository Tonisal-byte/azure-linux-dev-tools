// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

// Package originhandlers provides per-[projectconfig.OriginType] handlers that
// resolve source files. Each handler is a function matching the [Handler]
// signature; new origin types are added by creating a constructor in a new file
// and registering it in the origin handler map built by the source manager.
package originhandlers

import (
	"context"

	"github.com/microsoft/azure-linux-dev-tools/internal/projectconfig"
)

// Handler resolves a source file for a specific [projectconfig.OriginType].
// It is called by the source manager after lookaside-cache resolution has
// been attempted (and failed or was skipped).
type Handler func(
	ctx context.Context,
	fileRef *projectconfig.SourceFileReference,
	destPath string,
) error
