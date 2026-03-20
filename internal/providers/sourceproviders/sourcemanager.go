// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package sourceproviders

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"path/filepath"

	"github.com/microsoft/azure-linux-dev-tools/internal/app/azldev"
	"github.com/microsoft/azure-linux-dev-tools/internal/app/azldev/core/components"
	"github.com/microsoft/azure-linux-dev-tools/internal/global/opctx"
	"github.com/microsoft/azure-linux-dev-tools/internal/projectconfig"
	"github.com/microsoft/azure-linux-dev-tools/internal/providers/sourceproviders/fedorasource"
	"github.com/microsoft/azure-linux-dev-tools/internal/utils/downloader"
	"github.com/microsoft/azure-linux-dev-tools/internal/utils/fileutils"
	"github.com/microsoft/azure-linux-dev-tools/internal/utils/git"
	"github.com/microsoft/azure-linux-dev-tools/internal/utils/retry"
)

//go:generate go tool -modfile=../../../tools/mockgen/go.mod mockgen -package=sourceproviders_test -destination=sourceproviders_test/sourcemanager_mocks.go github.com/microsoft/azure-linux-dev-tools/internal/providers/sourceproviders SourceManager

// Provider is an abstract interface implemented by a source provider.
type Provider interface{}

// FileSourceProvider is an abstract interface implemented by a source provider that can retrieve individual
// source files.
type FileSourceProvider interface {
	Provider

	// GetFiles retrieves the specified source files and places them in the provided directory. If a file
	// is not known to (or handled by) the providers, the error will be (or will wrap) ErrNotFound.
	GetFiles(ctx context.Context, fileRefs []projectconfig.SourceFileReference, destDirPath string) error
}

// SourceIdentityProvider resolves a reproducible identity string for a component's source.
// The identity changes whenever the source content would change — the exact representation
// depends on the source type (e.g., a commit hash for dist-git, a content hash for local files).
//
// Consumers should treat the returned string as opaque; it is only meaningful for equality
// comparison between two runs.
type SourceIdentityProvider interface {
	// ResolveIdentity returns a deterministic identity string for the component's source.
	// Returns an error if the identity cannot be determined (e.g., network failure for upstream sources).
	// Upstream components must return the resolved commit hash from the dist-git provider, local components
	// must return a content hash of the spec directory (must be stable, but exact format and algorithm
	// are up to the provider).
	ResolveIdentity(ctx context.Context, component components.Component) (string, error)
}

// FetchComponentOptions holds optional parameters for component fetching operations.
type FetchComponentOptions struct {
	// PreserveGitDir, when true, instructs the provider to keep the upstream .git directory
	// in the fetched component sources instead of deleting it. This is required for building
	// synthetic git history from overlay blame metadata.
	PreserveGitDir bool

	// SkipLookaside, when true, skips all lookaside cache downloads during component
	// fetching. Git-tracked files (spec, patches, scripts) are still fetched from the
	// upstream clone. The sources manifest file remains available for validation.
	SkipLookaside bool
}

// FetchComponentOption is a functional option for configuring component fetch behavior.
type FetchComponentOption func(*FetchComponentOptions)

// WithPreserveGitDir returns a [FetchComponentOption] that instructs the provider to preserve
// the upstream .git directory in the fetched component sources.
func WithPreserveGitDir() FetchComponentOption {
	return func(o *FetchComponentOptions) {
		o.PreserveGitDir = true
	}
}

// WithSkipLookaside returns a [FetchComponentOption] that skips lookaside cache
// downloads during component fetching. Git-tracked files are still fetched.
func WithSkipLookaside() FetchComponentOption {
	return func(o *FetchComponentOptions) {
		o.SkipLookaside = true
	}
}

// resolveFetchComponentOptions applies all functional options and returns the resolved options.
func resolveFetchComponentOptions(opts []FetchComponentOption) FetchComponentOptions {
	var resolved FetchComponentOptions

	for _, opt := range opts {
		if opt == nil {
			continue
		}

		opt(&resolved)
	}

	return resolved
}

// ComponentSourceProvider is an abstract interface implemented by a source provider that can retrieve the
// full file contents of a given component or calculate an identity.
type ComponentSourceProvider interface {
	Provider
	SourceIdentityProvider

	// GetComponent retrieves the `.spec` for the specified component along with any sidecar
	// files stored along with it, placing the fetched files in the provided directory.
	GetComponent(
		ctx context.Context, component components.Component, destDirPath string,
		opts ...FetchComponentOption,
	) error
}

// SourceManager is an abstract interface for a facility that can fetch arbitrary component sources.
type SourceManager interface {
	// FetchFiles fetches the given source files, placing the files in the provided directory.
	FetchFiles(ctx context.Context, component components.Component, destDirPath string) error

	// FetchComponent fetches an entire upstream component, including its `.spec` file and any sidecar files.
	// Optional [FetchComponentOption] values may be passed to control provider behavior (e.g., preserving
	// the upstream .git directory).
	FetchComponent(
		ctx context.Context, component components.Component, destDirPath string,
		opts ...FetchComponentOption,
	) error

	// ResolveSourceIdentity returns a deterministic identity string for the component's source.
	// For local components, this is a content hash of the spec directory.
	// For upstream components, this is the resolved commit hash from the dist-git provider.
	ResolveSourceIdentity(ctx context.Context, component components.Component) (string, error)
}

// ResolvedDistro holds the fully resolved distro configuration for a component.
// This is resolved once at the call site and passed through the source manager
// to providers, so each consumer can derive only what it needs.
type ResolvedDistro struct {
	// Ref is the effective distro reference (component override or project default).
	// Contains the snapshot time used for commit selection.
	Ref projectconfig.DistroReference

	// Definition is the resolved distro definition containing base URIs.
	Definition projectconfig.DistroDefinition

	// Version is the resolved distro version definition containing branch info.
	Version projectconfig.DistroVersionDefinition
}

// ResolveDistro resolves the effective distro for a component, falling back to
// the project's default distro when the component doesn't specify one.
// Returns an error if no effective distro can be resolved.
func ResolveDistro(env *azldev.Env, component components.Component) (ResolvedDistro, error) {
	ref := component.GetConfig().Spec.UpstreamDistro
	if ref.Name == "" {
		ref = env.Config().Project.DefaultDistro
	}

	if ref.Name == "" {
		return ResolvedDistro{}, fmt.Errorf(
			"no distro configured for component %#q"+
				" (set upstream-distro on the component or default-distro on the project)",
			component.GetName(),
		)
	}

	distroDef, distroVersionDef, err := env.ResolveDistroRef(ref)
	if err != nil {
		return ResolvedDistro{}, fmt.Errorf("failed to resolve distro %#q:\n%w", ref.Name, err)
	}

	return ResolvedDistro{
		Ref:        ref,
		Definition: distroDef,
		Version:    distroVersionDef,
	}, nil
}

type sourceManager struct {
	// Upstream component providers (can have multiple, e.g., different RPM repos)
	upstreamComponentProviders []ComponentSourceProvider

	// File providers for individual files
	fileProviders []FileSourceProvider

	// Lookaside downloader for fetching source tarballs from upstream caches
	lookasideDownloader fedorasource.FedoraSourceDownloader

	// Retry configuration for network operations
	retryConfig retry.Config

	// Dependencies extracted from environment
	dryRunnable      opctx.DryRunnable
	eventListener    opctx.EventListener
	cmdFactory       opctx.CmdFactory
	fs               opctx.FS
	lookasideBaseURI string
	disableOrigins   bool
}

var _ SourceManager = (*sourceManager)(nil)

func NewSourceManager(env *azldev.Env, distro ResolvedDistro) (SourceManager, error) {
	if env == nil {
		return nil, errors.New("environment cannot be nil")
	}

	// Build retry config from environment
	retryCfg := retry.DefaultConfig()
	if env.NetworkRetries() > 0 {
		retryCfg.MaxAttempts = env.NetworkRetries()
	}

	// Extract dependencies from environment
	manager := &sourceManager{
		upstreamComponentProviders: make([]ComponentSourceProvider, 0),
		fileProviders:              make([]FileSourceProvider, 0),
		retryConfig:                retryCfg,
		dryRunnable:                env,
		eventListener:              env,
		cmdFactory:                 env,
		fs:                         env.FS(),
		lookasideBaseURI:           distro.Definition.LookasideBaseURI,
		disableOrigins:             distro.Definition.DisableOrigins,
	}

	// Create lookaside downloader for fetching source tarballs
	err := manager.createLookasideDownloader()
	if err != nil {
		slog.Warn("Failed to create lookaside downloader; lookaside downloads will be disabled", "error", err)
	}

	// Create component providers
	manager.createComponentProviders(distro)

	// Ensure at least one provider was created successfully
	if len(manager.upstreamComponentProviders) == 0 &&
		len(manager.fileProviders) == 0 {
		slog.Debug("No upstream source providers could be created; only local components will be supported")
	}

	return manager, nil
}

// createComponentProviders creates all component providers we may need.
// Failures are logged as warnings rather than propagated, so that local-only
// builds can proceed. Upstream fetches will fail at runtime with a clear error
// if no providers were registered.
func (m *sourceManager) createComponentProviders(distro ResolvedDistro) {
	// Create Fedora component provider with all required dependencies
	fedoraProvider, err := m.createFedoraContentsProvider(distro)
	if err != nil {
		slog.Warn("Failed to setup Fedora component provider; upstream component fetches will not be available",
			"error", err)

		return
	}

	m.upstreamComponentProviders = append(m.upstreamComponentProviders, fedoraProvider)

	slog.Debug("Registered Fedora component provider")
}

func (m *sourceManager) FetchFiles(
	ctx context.Context,
	component components.Component,
	destDirPath string,
) error {
	sourceFiles := component.GetConfig().SourceFiles
	if len(sourceFiles) == 0 {
		slog.Debug("No source files to fetch for component", "component", component.GetName())

		return nil
	}

	for i := range sourceFiles {
		fileRef := &sourceFiles[i]

		err := m.fetchSourceFile(ctx, component, fileRef, destDirPath)
		if err != nil {
			return fmt.Errorf("failed to fetch source file %#q:\n%w", fileRef.Filename, err)
		}
	}

	return nil
}

// fetchSourceFile acquires a single source file by delegating to the appropriate
// [originHandler] for the file's origin type. Download-type origins check for
// existing files first; generative origins always regenerate.
func (m *sourceManager) fetchSourceFile(
	ctx context.Context,
	component components.Component,
	fileRef *projectconfig.SourceFileReference,
	destDirPath string,
) error {
	// Validate filename to prevent path traversal vulnerabilities.
	if err := fileutils.ValidateFilename(fileRef.Filename); err != nil {
		return fmt.Errorf("invalid source file reference:\n%w", err)
	}

	destPath := filepath.Join(destDirPath, fileRef.Filename)

	// For non-generative origins, skip if the file already exists.
	if !isGenerativeOrigin(fileRef.Origin.Type) {
		sourceExists, err := fileutils.Exists(m.fs, destPath)
		if err != nil {
			return fmt.Errorf("failed to check existence of destination file %#q:\n%w", destPath, err)
		}

		if sourceExists {
			slog.Debug("Source file already exists, skipping",
				"filename", fileRef.Filename,
				"path", destPath)

			return nil
		}
	}

	// Resolve the handler for this origin type and delegate.
	handler, err := m.resolveOriginHandler(fileRef.Origin.Type)
	if err != nil {
		return fmt.Errorf("failed to resolve handler for source file %#q:\n%w", fileRef.Filename, err)
	}

	err = handler.Handle(ctx, component, fileRef, destPath, destDirPath)
	if err != nil {
		return fmt.Errorf("origin handler failed for source file %#q:\n%w", fileRef.Filename, err)
	}

	return nil
}

// resolvePackageName determines the package name to use for lookaside lookups.
// It uses the component's upstream name if set, otherwise falls back to the component name.
func resolvePackageName(component components.Component) string {
	if upstreamName := component.GetConfig().Spec.UpstreamName; upstreamName != "" {
		return upstreamName
	}

	return component.GetName()
}

func (m *sourceManager) FetchComponent(
	ctx context.Context, component components.Component, destDirPath string, opts ...FetchComponentOption,
) error {
	if component.GetName() == "" {
		return errors.New("component name is empty")
	}

	sourceType := component.GetConfig().Spec.SourceType

	resolved := resolveFetchComponentOptions(opts)

	switch sourceType {
	case projectconfig.SpecSourceTypeLocal, projectconfig.SpecSourceTypeUnspecified:
		return m.fetchLocalComponent(ctx, component, destDirPath, resolved)

	case projectconfig.SpecSourceTypeUpstream:
		return m.fetchUpstreamComponent(ctx, component, destDirPath, opts...)
	}

	return fmt.Errorf("spec for component %#q not found in any configured provider",
		component.GetName())
}

func (m *sourceManager) ResolveSourceIdentity(
	ctx context.Context, component components.Component,
) (string, error) {
	if component.GetName() == "" {
		return "", errors.New("component name is empty")
	}

	sourceType := component.GetConfig().Spec.SourceType

	switch sourceType {
	case projectconfig.SpecSourceTypeLocal, projectconfig.SpecSourceTypeUnspecified:
		specPath := component.GetConfig().Spec.Path
		if specPath == "" {
			return "", fmt.Errorf("component %#q has no spec path configured", component.GetName())
		}

		return ResolveLocalSourceIdentity(m.fs, filepath.Dir(specPath))

	case projectconfig.SpecSourceTypeUpstream:
		return m.resolveUpstreamSourceIdentity(ctx, component)
	}

	return "", fmt.Errorf("no identity provider for source type %#q on component %#q",
		sourceType, component.GetName())
}

func (m *sourceManager) resolveUpstreamSourceIdentity(
	ctx context.Context, component components.Component,
) (string, error) {
	if len(m.upstreamComponentProviders) == 0 {
		return "", fmt.Errorf("no upstream providers configured for component %#q",
			component.GetName())
	}

	var lastError error

	for _, provider := range m.upstreamComponentProviders {
		identity, err := provider.ResolveIdentity(ctx, component)
		if err == nil {
			return identity, nil
		}

		lastError = err
	}

	return "", fmt.Errorf("failed to resolve source identity for upstream component %#q:\n%w",
		component.GetName(), lastError)
}

func (m *sourceManager) fetchLocalComponent(
	ctx context.Context, component components.Component, destDirPath string, opts FetchComponentOptions,
) error {
	err := FetchLocalComponent(m.dryRunnable, m.eventListener, m.fs, component, destDirPath, false)
	if err != nil {
		return fmt.Errorf("failed to fetch local component %#q:\n%w",
			component.GetName(), err)
	}

	// Download source files from lookaside cache if available.
	// Skip this step when SkipLookaside is set (e.g., during rendering).
	if !opts.SkipLookaside {
		err = m.downloadLookasideSources(ctx, component, destDirPath)
		if err != nil {
			return fmt.Errorf("failed to download lookaside sources for component %#q:\n%w",
				component.GetName(), err)
		}
	}

	return nil
}

// downloadLookasideSources downloads source tarballs from a lookaside cache for the given component.
// It resolves the appropriate lookaside URI from the distro configuration and uses the component's
// upstream name (if set) as the package name for the lookaside lookup.
// Returns nil if no lookaside downloader or URI is available.
func (m *sourceManager) downloadLookasideSources(
	ctx context.Context, component components.Component, destDirPath string,
) error {
	if m.lookasideDownloader == nil {
		return nil
	}

	if m.lookasideBaseURI == "" {
		return nil
	}

	packageName := resolvePackageName(component)

	err := m.lookasideDownloader.ExtractSourcesFromRepo(ctx, destDirPath, packageName, m.lookasideBaseURI, nil)
	if err != nil {
		return fmt.Errorf("failed to extract sources from lookaside cache:\n%w", err)
	}

	return nil
}

func (m *sourceManager) fetchUpstreamComponent(
	ctx context.Context, component components.Component, destDirPath string, opts ...FetchComponentOption,
) error {
	if len(m.upstreamComponentProviders) == 0 {
		return fmt.Errorf("no upstream component origins configured for component %#q",
			component.GetName())
	}

	var lastError error

	// Try each upstream component provider, until one succeeds
	for _, provider := range m.upstreamComponentProviders {
		err := provider.GetComponent(ctx, component, destDirPath, opts...)
		if err == nil {
			slog.Debug("Successfully fetched upstream component",
				"component", component.GetName(),
				"provider", fmt.Sprintf("%T", provider))

			return nil
		}

		lastError = err
	}

	// If we tried providers but none succeeded
	return fmt.Errorf("failed to fetch upstream component %#q:\n%w",
		component.GetName(), lastError)
}

func (m *sourceManager) createLookasideDownloader() error {
	httpDownloader, err := downloader.NewHTTPDownloader(m.dryRunnable, m.eventListener, m.fs)
	if err != nil {
		return fmt.Errorf("failed to create HTTP downloader:\n%w", err)
	}

	extractor, err := fedorasource.NewFedoraRepoExtractorImpl(
		m.dryRunnable,
		m.fs,
		httpDownloader,
		m.retryConfig,
	)
	if err != nil {
		return fmt.Errorf("failed to create lookaside downloader:\n%w", err)
	}

	m.lookasideDownloader = extractor

	return nil
}

func (m *sourceManager) createFedoraContentsProvider(distro ResolvedDistro) (*FedoraSourcesProviderImpl, error) {
	gitProvider, err := git.NewGitProviderImpl(m.eventListener, m.cmdFactory)
	if err != nil {
		return nil, fmt.Errorf("failed to create git provider:\n%w", err)
	}

	if m.lookasideDownloader == nil {
		return nil, errors.New("lookaside downloader is required for Git component provider")
	}

	return NewFedoraSourcesProviderImpl(
		m.fs,
		m.dryRunnable,
		gitProvider,
		m.lookasideDownloader,
		distro,
		m.retryConfig,
	)
}
