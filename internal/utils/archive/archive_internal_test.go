// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package archive

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestExtract_OversizedEntryRejected(t *testing.T) {
	previous := maxEntryBytes
	maxEntryBytes = 5

	t.Cleanup(func() { maxEntryBytes = previous })

	tmpDir := t.TempDir()
	archivePath := filepath.Join(tmpDir, "big.tar.gz")
	extractDir := filepath.Join(tmpDir, "out")
	require.NoError(t, os.MkdirAll(extractDir, 0o755))

	var buf bytes.Buffer

	gzWriter := gzip.NewWriter(&buf)
	tarWriter := tar.NewWriter(gzWriter)

	const content = "hello world" // 11 bytes > 5

	require.NoError(t, tarWriter.WriteHeader(&tar.Header{
		Name:     "pkg/huge.bin",
		Typeflag: tar.TypeReg,
		Mode:     0o644,
		Size:     int64(len(content)),
	}))
	_, err := tarWriter.Write([]byte(content))
	require.NoError(t, err)
	require.NoError(t, tarWriter.Close())
	require.NoError(t, gzWriter.Close())
	require.NoError(t, os.WriteFile(archivePath, buf.Bytes(), 0o600))

	err = Extract(archivePath, extractDir, CompressionGzip)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "exceeds max of 5")

	_, statErr := os.Stat(filepath.Join(extractDir, "pkg", "huge.bin"))
	assert.True(t, os.IsNotExist(statErr), "oversized entry must not leave a partial file (stat err=%v)", statErr)
}

func TestExtract_TotalSizeLimitRejected(t *testing.T) {
	previous := maxTotalBytes
	maxTotalBytes = 10

	t.Cleanup(func() { maxTotalBytes = previous })

	tmpDir := t.TempDir()
	archivePath := filepath.Join(tmpDir, "too-large.tar.gz")
	extractDir := filepath.Join(tmpDir, "out")

	createTestTarGz(t, archivePath, []tarEntry{
		{name: "first.txt", content: "hello"},
		{name: "second.txt", content: "world!"},
	})

	err := Extract(archivePath, extractDir, CompressionGzip)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "total extraction limit of 10 bytes")

	content, readErr := os.ReadFile(filepath.Join(extractDir, "first.txt"))
	require.NoError(t, readErr)
	assert.Equal(t, "hello", string(content))

	_, statErr := os.Stat(filepath.Join(extractDir, "second.txt"))
	assert.True(t, os.IsNotExist(statErr), "entry exceeding total limit must not be extracted (stat err=%v)", statErr)
}

func TestExtract_EntryCountLimitRejected(t *testing.T) {
	previous := maxEntries
	maxEntries = 2

	t.Cleanup(func() { maxEntries = previous })

	tmpDir := t.TempDir()
	archivePath := filepath.Join(tmpDir, "too-many-entries.tar.gz")
	extractDir := filepath.Join(tmpDir, "out")

	createTestTarGz(t, archivePath, []tarEntry{
		{name: "first.txt", content: "1"},
		{name: "second.txt", content: "2"},
		{name: "third.txt", content: "3"},
	})

	err := Extract(archivePath, extractDir, CompressionGzip)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "more than 2 entries")

	_, statErr := os.Stat(filepath.Join(extractDir, "third.txt"))
	assert.True(t, os.IsNotExist(statErr), "entry exceeding count limit must not be extracted (stat err=%v)", statErr)
}

type tarEntry struct {
	name    string
	content string
}

func createTestTarGz(t *testing.T, path string, entries []tarEntry) {
	t.Helper()

	var buf bytes.Buffer

	gzWriter := gzip.NewWriter(&buf)
	tarWriter := tar.NewWriter(gzWriter)

	for _, entry := range entries {
		require.NoError(t, tarWriter.WriteHeader(&tar.Header{
			Name:     entry.name,
			Typeflag: tar.TypeReg,
			Mode:     0o644,
			Size:     int64(len(entry.content)),
		}))
		_, err := tarWriter.Write([]byte(entry.content))
		require.NoError(t, err)
	}

	require.NoError(t, tarWriter.Close())
	require.NoError(t, gzWriter.Close())
	require.NoError(t, os.WriteFile(path, buf.Bytes(), 0o600))
}
