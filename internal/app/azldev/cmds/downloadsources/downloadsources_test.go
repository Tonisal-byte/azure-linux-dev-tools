// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package downloadsources_test

import (
	"testing"

	"github.com/microsoft/azure-linux-dev-tools/internal/app/azldev"
	"github.com/microsoft/azure-linux-dev-tools/internal/app/azldev/cmds/downloadsources"
	"github.com/microsoft/azure-linux-dev-tools/internal/app/azldev/core/testutils"
	"github.com/microsoft/azure-linux-dev-tools/internal/global/opctx/opctx_test"
	"github.com/microsoft/azure-linux-dev-tools/internal/projectconfig"
	"github.com/microsoft/azure-linux-dev-tools/internal/utils/fileperms"
	"github.com/microsoft/azure-linux-dev-tools/internal/utils/fileutils"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.uber.org/mock/gomock"
)

func TestOnAppInit(t *testing.T) {
	ctrl := gomock.NewController(t)
	app := azldev.NewApp(opctx_test.NewMockFileSystemFactory(ctrl), opctx_test.NewMockOSEnvFactory(ctrl))

	downloadsources.OnAppInit(app)

	topLevelCommandNames, err := app.CommandNames()
	require.NoError(t, err)
	assert.Contains(t, topLevelCommandNames, "download-sources [directory]")
}

func TestNewDownloadSourcesCmd(t *testing.T) {
	cmd := downloadsources.NewDownloadSourcesCmd()
	require.NotNil(t, cmd)
	assert.Equal(t, "download-sources [directory]", cmd.Use)

	outputDirFlag := cmd.Flags().Lookup("output-dir")
	require.NotNil(t, outputDirFlag, "--output-dir flag should be registered")
	assert.Equal(t, "o", outputDirFlag.Shorthand)
	assert.Empty(t, outputDirFlag.DefValue)

	componentFlag := cmd.Flags().Lookup("component")
	require.NotNil(t, componentFlag, "--component flag should be registered")
}

func TestResolveFromSpecFile_SingleSpec(t *testing.T) {
	testEnv := testutils.NewTestEnv(t)

	// Create a directory with a spec file and sources file.
	specDir := "/project/testpkg"
	require.NoError(t, fileutils.MkdirAll(testEnv.TestFS, specDir))
	require.NoError(t, fileutils.WriteFile(
		testEnv.TestFS, specDir+"/curl.spec", []byte("Name: curl\n"), fileperms.PrivateFile))
	require.NoError(t, fileutils.WriteFile(testEnv.TestFS, specDir+"/sources", []byte(""), fileperms.PrivateFile))

	options := &downloadsources.DownloadSourcesOptions{
		Directory: specDir,
	}

	// This will fail at the download step (no real HTTP), but we can verify
	// it gets past parameter resolution by checking the error message.
	err := downloadsources.DownloadSources(testEnv.Env, options)
	// Should not fail with "no .spec file" or "no lookaside" errors.
	// It should fail at the download stage since there are no real sources to download.
	// The absence of a sources-related resolution error confirms resolveFromSpecFile worked.
	if err != nil {
		assert.NotContains(t, err.Error(), "no .spec file found")
		assert.NotContains(t, err.Error(), "no lookaside base URI")
	}
}

func TestResolveFromSpecFile_NoSpec(t *testing.T) {
	testEnv := testutils.NewTestEnv(t)

	specDir := "/project/empty"
	require.NoError(t, fileutils.MkdirAll(testEnv.TestFS, specDir))

	options := &downloadsources.DownloadSourcesOptions{
		Directory: specDir,
	}

	err := downloadsources.DownloadSources(testEnv.Env, options)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "no .spec file found")
}

func TestResolveFromSpecFile_MultipleSpecs(t *testing.T) {
	testEnv := testutils.NewTestEnv(t)

	specDir := "/project/multi"
	require.NoError(t, fileutils.MkdirAll(testEnv.TestFS, specDir))
	require.NoError(t, fileutils.WriteFile(testEnv.TestFS, specDir+"/a.spec", []byte(""), fileperms.PrivateFile))
	require.NoError(t, fileutils.WriteFile(testEnv.TestFS, specDir+"/b.spec", []byte(""), fileperms.PrivateFile))

	options := &downloadsources.DownloadSourcesOptions{
		Directory: specDir,
	}

	err := downloadsources.DownloadSources(testEnv.Env, options)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "multiple .spec files found")
}

func TestResolveLookasideURI_FollowsUpstreamDistro(t *testing.T) {
	testEnv := testutils.NewTestEnv(t)

	// Reconfigure: default distro has NO lookaside URI, but points to an upstream that does.
	testEnv.Config.Distros["test-distro"] = projectconfig.DistroDefinition{
		Versions: map[string]projectconfig.DistroVersionDefinition{
			"1.0": {
				DefaultComponentConfig: projectconfig.ComponentConfig{
					Spec: projectconfig.SpecSource{
						UpstreamDistro: projectconfig.DistroReference{
							Name:    "upstream-distro",
							Version: "42",
						},
					},
				},
			},
		},
	}

	testEnv.Config.Distros["upstream-distro"] = projectconfig.DistroDefinition{
		LookasideBaseURI: "https://upstream.example.com/lookaside/$pkg/$filename/$hashtype/$hash/$filename",
		Versions: map[string]projectconfig.DistroVersionDefinition{
			"42": {},
		},
	}

	specDir := "/project/testpkg"
	require.NoError(t, fileutils.MkdirAll(testEnv.TestFS, specDir))
	require.NoError(t, fileutils.WriteFile(testEnv.TestFS, specDir+"/mypkg.spec", []byte(""), fileperms.PrivateFile))
	require.NoError(t, fileutils.WriteFile(testEnv.TestFS, specDir+"/sources", []byte(""), fileperms.PrivateFile))

	options := &downloadsources.DownloadSourcesOptions{
		Directory: specDir,
	}

	err := downloadsources.DownloadSources(testEnv.Env, options)
	// Should not fail with lookaside resolution errors.
	if err != nil {
		assert.NotContains(t, err.Error(), "no lookaside base URI")
		assert.NotContains(t, err.Error(), "no upstream distro reference found")
	}
}
