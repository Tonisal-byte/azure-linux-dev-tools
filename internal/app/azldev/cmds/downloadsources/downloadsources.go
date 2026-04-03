// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package downloadsources

import (
	"errors"
	"fmt"
	"log/slog"
	"path/filepath"
	"strings"

	"github.com/bmatcuk/doublestar/v4"
	"github.com/microsoft/azure-linux-dev-tools/internal/app/azldev"
	"github.com/microsoft/azure-linux-dev-tools/internal/app/azldev/core/components"
	"github.com/microsoft/azure-linux-dev-tools/internal/providers/sourceproviders"
	"github.com/microsoft/azure-linux-dev-tools/internal/providers/sourceproviders/fedorasource"
	"github.com/microsoft/azure-linux-dev-tools/internal/utils/downloader"
	"github.com/microsoft/azure-linux-dev-tools/internal/utils/fileutils"
	"github.com/microsoft/azure-linux-dev-tools/internal/utils/retry"
	"github.com/spf13/cobra"
)

// specFileExtension is the file extension for RPM spec files.
const specFileExtension = ".spec"

// DownloadSourcesOptions holds the options for the download-sources command.
type DownloadSourcesOptions struct {
	ComponentFilter components.ComponentFilter
	Directory       string
	OutputDir       string
}

// OnAppInit registers the download-sources command as a top-level command.
func OnAppInit(app *azldev.App) {
	app.AddTopLevelCommand(NewDownloadSourcesCmd())
}

// NewDownloadSourcesCmd creates the download-sources cobra command.
func NewDownloadSourcesCmd() *cobra.Command {
	var options DownloadSourcesOptions

	cmd := &cobra.Command{
		Use:   "download-sources [directory]",
		Short: "Download source files listed in a Fedora-format sources file",
		Long: `Download source files from a lookaside cache based on a Fedora-format
'sources' file in the specified directory.

The command reads the 'sources' file, resolves the lookaside cache URI from
the distro configuration, and downloads each listed file into the directory.
Files that already exist in the directory are skipped.

The package name is derived from the .spec file in the directory. If a
component is specified, its upstream-name override and distro configuration
are used instead. When no component is given, the project's default distro
is used.`,
		Example: `  # Download sources (package name derived from .spec file in directory)
  azldev download-sources ./path/to/sources/dir

  # Download sources in the current directory
  azldev download-sources

  # Download sources for a specific component
  azldev download-sources ./path/to/sources/dir -p curl

  # Download sources to a different output directory
  azldev download-sources ./path/to/sources/dir -o /tmp/output`,
		Args: cobra.MaximumNArgs(1),
		RunE: azldev.RunFuncWithExtraArgs(func(env *azldev.Env, args []string) (interface{}, error) {
			options.Directory = "."
			if len(args) > 0 {
				options.Directory = args[0]
			}

			return nil, DownloadSources(env, &options)
		}),
		ValidArgsFunction: func(
			_ *cobra.Command, _ []string, _ string,
		) ([]string, cobra.ShellCompDirective) {
			return nil, cobra.ShellCompDirectiveFilterDirs
		},
	}

	components.AddComponentFilterOptionsToCommand(cmd, &options.ComponentFilter)

	cmd.Flags().StringVarP(&options.OutputDir, "output-dir", "o", "",
		"output directory for downloaded files (defaults to source directory)")
	_ = cmd.MarkFlagDirname("output-dir")

	azldev.ExportAsMCPTool(cmd)

	return cmd
}

// DownloadSources downloads source files from a lookaside cache into the specified directory.
func DownloadSources(env *azldev.Env, options *DownloadSourcesOptions) error {
	packageName, lookasideBaseURI, err := resolveDownloadParams(env, options)
	if err != nil {
		return err
	}

	slog.Info("Downloading sources from lookaside cache",
		"packageName", packageName,
		"outputDir", options.OutputDir,
	)

	// Build retry config from environment.
	retryCfg := retry.DefaultConfig()
	if env.NetworkRetries() > 0 {
		retryCfg.MaxAttempts = env.NetworkRetries()
	}

	// Create the HTTP downloader and lookaside source downloader.
	httpDownloader, err := downloader.NewHTTPDownloader(env, env, env.FS())
	if err != nil {
		return fmt.Errorf("failed to create HTTP downloader:\n%w", err)
	}

	lookasideDownloader, err := fedorasource.NewFedoraRepoExtractorImpl(
		env, env.FS(), httpDownloader, retryCfg,
	)
	if err != nil {
		return fmt.Errorf("failed to create lookaside downloader:\n%w", err)
	}

	// Determine where to download files.
	downloadDir := options.Directory
	if options.OutputDir != "" {
		downloadDir = options.OutputDir
	}

	// If downloading to a different directory, copy the sources file there.
	if downloadDir != options.Directory {
		srcPath := filepath.Join(options.Directory, "sources")
		dstPath := filepath.Join(downloadDir, "sources")

		if err := fileutils.MkdirAll(env.FS(), downloadDir); err != nil {
			return fmt.Errorf("failed to create output directory %#q:\n%w", downloadDir, err)
		}

		if err := fileutils.CopyFile(env, env.FS(), srcPath, dstPath, fileutils.CopyFileOptions{}); err != nil {
			return fmt.Errorf("failed to copy sources file to output directory:\n%w", err)
		}
	}

	// Download all sources listed in the sources file.
	err = lookasideDownloader.ExtractSourcesFromRepo(
		env, downloadDir, packageName, lookasideBaseURI, nil,
	)
	if err != nil {
		return fmt.Errorf("failed to download sources:\n%w", err)
	}

	slog.Info("Sources downloaded successfully", "outputDir", options.OutputDir)

	return nil
}

// resolveDownloadParams determines the package name and lookaside URI, either from
// a specified component or by reading the .spec file in the directory.
func resolveDownloadParams(
	env *azldev.Env, options *DownloadSourcesOptions,
) (packageName string, lookasideBaseURI string, err error) {
	if !options.ComponentFilter.HasNoCriteria() {
		return resolveFromComponent(env, options)
	}

	return resolveFromSpecFile(env, options.Directory)
}

// resolveFromComponent resolves the package name and lookaside URI from a component's config.
func resolveFromComponent(
	env *azldev.Env, options *DownloadSourcesOptions,
) (packageName string, lookasideBaseURI string, err error) {
	resolver := components.NewResolver(env)

	comps, err := resolver.FindComponents(&options.ComponentFilter)
	if err != nil {
		return "", "", fmt.Errorf("failed to resolve components:\n%w", err)
	}

	if comps.Len() == 0 {
		return "", "", errors.New("no components were selected; " +
			"please use command-line options to indicate which component to use")
	}

	if comps.Len() != 1 {
		return "", "", fmt.Errorf("expected exactly one component, got %d", comps.Len())
	}

	component := comps.Components()[0]

	packageName = component.GetName()
	if upstreamName := component.GetConfig().Spec.UpstreamName; upstreamName != "" {
		packageName = upstreamName
	}

	distro, err := sourceproviders.ResolveDistro(env, component)
	if err != nil {
		return "", "", fmt.Errorf("failed to resolve distro for component %#q:\n%w", component.GetName(), err)
	}

	lookasideBaseURI = distro.Definition.LookasideBaseURI
	if lookasideBaseURI == "" {
		return "", "", fmt.Errorf("no lookaside base URI configured for distro %#q", distro.Ref.Name)
	}

	return packageName, lookasideBaseURI, nil
}

// resolveFromSpecFile derives the package name from a .spec file in the directory
// and uses the project's default distro for the lookaside URI.
func resolveFromSpecFile(
	env *azldev.Env, directory string,
) (packageName string, lookasideBaseURI string, err error) {
	dirExists, err := fileutils.DirExists(env.FS(), directory)
	if err != nil {
		return "", "", fmt.Errorf("failed to check directory %#q:\n%w", directory, err)
	}

	if !dirExists {
		return "", "", fmt.Errorf("directory %#q does not exist", directory)
	}

	specPattern := filepath.Join(directory, "*"+specFileExtension)

	specFiles, err := fileutils.Glob(env.FS(), specPattern, doublestar.WithFilesOnly())
	if err != nil {
		return "", "", fmt.Errorf("failed to search for spec files in %#q:\n%w", directory, err)
	}

	if len(specFiles) == 0 {
		return "", "", fmt.Errorf("no .spec file found in %#q; "+
			"specify a component with -p to provide the package name", directory)
	}

	if len(specFiles) > 1 {
		return "", "", fmt.Errorf("multiple .spec files found in %#q; "+
			"specify a component with -p to select one", directory)
	}

	packageName = strings.TrimSuffix(filepath.Base(specFiles[0]), specFileExtension)

	slog.Debug("Derived package name from spec filename", "name", packageName, "specFile", specFiles[0])

	lookasideBaseURI, err = resolveLookasideURI(env)
	if err != nil {
		return "", "", err
	}

	return packageName, lookasideBaseURI, nil
}

// resolveLookasideURI finds the lookaside base URI by checking the default distro first,
// then following the upstream distro reference if needed.
func resolveLookasideURI(env *azldev.Env) (string, error) {
	distroDef, distroVersionDef, err := env.Distro()
	if err != nil {
		return "", fmt.Errorf("failed to resolve default distro:\n%w", err)
	}

	// If the default distro itself has a lookaside URI, use it directly.
	if distroDef.LookasideBaseURI != "" {
		return distroDef.LookasideBaseURI, nil
	}

	// Otherwise, follow the upstream distro reference from the default component config.
	upstreamRef := distroVersionDef.DefaultComponentConfig.Spec.UpstreamDistro
	if upstreamRef.Name == "" {
		return "", errors.New("no lookaside base URI configured for the default distro, " +
			"and no upstream distro reference found; specify a component with -p")
	}

	upstreamDef, _, err := env.ResolveDistroRef(upstreamRef)
	if err != nil {
		return "", fmt.Errorf("failed to resolve upstream distro %#q:\n%w", upstreamRef.Name, err)
	}

	if upstreamDef.LookasideBaseURI == "" {
		return "", fmt.Errorf("no lookaside base URI configured for upstream distro %#q", upstreamRef.Name)
	}

	return upstreamDef.LookasideBaseURI, nil
}
