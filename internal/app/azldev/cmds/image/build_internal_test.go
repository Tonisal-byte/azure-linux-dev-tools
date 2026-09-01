// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package image

import (
	"context"
	"os/exec"
	"slices"
	"testing"

	"github.com/microsoft/azure-linux-dev-tools/internal/app/azldev/core/testutils"
	"github.com/microsoft/azure-linux-dev-tools/internal/projectconfig"
	"github.com/microsoft/azure-linux-dev-tools/internal/utils/fileperms"
	"github.com/microsoft/azure-linux-dev-tools/internal/utils/fileutils"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestCreateKiwiRunnerDistroConfigOverride(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name                 string
		configOverridePath   string
		createDistroOverride bool
		createOverrideDir    bool
		wantConfigPath       string
		wantError            string
	}{
		{
			name:                 "uses distro override",
			configOverridePath:   "/project/distro/kiwi/azl4/stage1/azurelinux-4.0-kiwi-override.yml",
			createDistroOverride: true,
			wantConfigPath:       "/project/distro/kiwi/azl4/stage1/azurelinux-4.0-kiwi-override.yml",
		},
		{
			name: "omits config without distro override",
		},
		{
			name:               "rejects inaccessible distro override",
			configOverridePath: "/project/distro/kiwi/missing.yml",
			wantError:          "file not found",
		},
		{
			name:               "rejects directory as distro override",
			configOverridePath: "/project/distro/kiwi/override.yml",
			createOverrideDir:  true,
			wantError:          "is a directory, expected a file",
		},
	}

	for _, testCase := range tests {
		t.Run(testCase.name, func(t *testing.T) {
			t.Parallel()

			testEnv := testutils.NewTestEnv(t)
			require.NoError(t, fileutils.MkdirAll(testEnv.TestFS, "/work"))

			distroName := testEnv.Config.Project.DefaultDistro.Name
			distro := testEnv.Config.Distros[distroName]
			versionName := testEnv.Config.Project.DefaultDistro.Version
			version := distro.Versions[versionName]
			version.KiwiConfigOverridePath = testCase.configOverridePath
			distro.Versions[versionName] = version
			testEnv.Config.Distros[distroName] = distro

			if testCase.createDistroOverride {
				require.NoError(t, fileutils.MkdirAll(testEnv.TestFS, "/project/distro/kiwi/azl4/stage1"))
				require.NoError(t, fileutils.WriteFile(
					testEnv.TestFS,
					testCase.configOverridePath,
					[]byte("custom: true\n"),
					fileperms.PrivateFile,
				))
			} else if testCase.createOverrideDir {
				require.NoError(t, fileutils.MkdirAll(testEnv.TestFS, testCase.configOverridePath))
			}

			var buildArgs []string

			testEnv.CmdFactory.RunHandler = func(cmd *exec.Cmd) error {
				buildArgs = cmd.Args

				return nil
			}

			runner, err := createKiwiRunner(
				testEnv.Env,
				&projectconfig.ImageConfig{
					Definition: projectconfig.ImageDefinition{Path: "/project/image/config.kiwi"},
				},
				"/work/output",
				&ImageBuildOptions{},
			)
			if testCase.wantError != "" {
				require.ErrorContains(t, err, testCase.wantError)

				return
			}

			require.NoError(t, err)
			require.NoError(t, runner.Build(context.Background()))

			configFlagIndex := slices.Index(buildArgs, "--config")
			if testCase.wantConfigPath == "" {
				assert.Equal(t, -1, configFlagIndex)
			} else {
				require.NotEqual(t, -1, configFlagIndex)
				require.Less(t, configFlagIndex+1, len(buildArgs))
				assert.Equal(t, testCase.wantConfigPath, buildArgs[configFlagIndex+1])
			}
		})
	}
}
