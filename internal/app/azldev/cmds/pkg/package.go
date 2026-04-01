// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package pkg

import (
	"github.com/microsoft/azure-linux-dev-tools/internal/app/azldev"
	"github.com/spf13/cobra"
)

// Called once when the app is initialized; registers any commands or callbacks with the app.
func OnAppInit(app *azldev.App) {
	cmd := &cobra.Command{
		Use:     "package",
		Aliases: []string{"pkg"},
		Short:   "Manage binary package configuration",
		Long: `Manage binary package configuration in an Azure Linux project.

Binary packages are the RPMs produced by building components. Use subcommands
to inspect and query the resolved configuration for packages, including metadata such as
publish channel assignments derived from package groups and component overrides.`,
	}

	app.AddTopLevelCommand(cmd)
	listOnAppInit(app, cmd)
}
