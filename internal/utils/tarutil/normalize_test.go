// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package tarutil_test

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"io"
	"testing"
	"time"

	"github.com/microsoft/azure-linux-dev-tools/internal/global/testctx"
	"github.com/microsoft/azure-linux-dev-tools/internal/utils/fileperms"
	"github.com/microsoft/azure-linux-dev-tools/internal/utils/fileutils"
	"github.com/microsoft/azure-linux-dev-tools/internal/utils/tarutil"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const testArchivePath = "/test/archive.tar.gz"

// createTestTarGz creates a .tar.gz archive in the in-memory filesystem with
// the given entries. Entries are written in the order provided, with
// intentionally non-deterministic metadata.
func createTestTarGz(t *testing.T, ctx *testctx.TestCtx, path string, entries []testEntry) {
	t.Helper()

	var buf bytes.Buffer

	gzWriter := gzip.NewWriter(&buf)
	gzWriter.Name = "original.tar"
	gzWriter.ModTime = time.Date(2025, 6, 15, 12, 0, 0, 0, time.UTC)
	gzWriter.OS = 3 // Unix

	tarWriter := tar.NewWriter(gzWriter)

	for _, entry := range entries {
		hdr := &tar.Header{
			Name:       entry.name,
			Mode:       entry.mode,
			Uid:        entry.uid,
			Gid:        entry.gid,
			Uname:      entry.uname,
			Gname:      entry.gname,
			Size:       int64(len(entry.data)),
			ModTime:    entry.modTime,
			AccessTime: entry.modTime,
			ChangeTime: entry.modTime,
			Typeflag:   entry.typeflag,
		}

		require.NoError(t, tarWriter.WriteHeader(hdr))

		if len(entry.data) > 0 {
			_, err := tarWriter.Write([]byte(entry.data))
			require.NoError(t, err)
		}
	}

	require.NoError(t, tarWriter.Close())
	require.NoError(t, gzWriter.Close())

	require.NoError(t, fileutils.WriteFile(ctx.FS(), path, buf.Bytes(), fileperms.PublicFile))
}

type testEntry struct {
	name     string
	mode     int64
	uid      int
	gid      int
	uname    string
	gname    string
	modTime  time.Time
	typeflag byte
	data     string
}

// readTarGzEntries reads all entries from the test archive in the in-memory filesystem.
func readTarGzEntries(t *testing.T, ctx *testctx.TestCtx) []tar.Header {
	t.Helper()

	data, err := fileutils.ReadFile(ctx.FS(), testArchivePath)
	require.NoError(t, err)

	gzReader, err := gzip.NewReader(bytes.NewReader(data))
	require.NoError(t, err)

	defer gzReader.Close()

	tarReader := tar.NewReader(gzReader)

	var headers []tar.Header

	for {
		hdr, err := tarReader.Next()
		if err == io.EOF {
			break
		}

		require.NoError(t, err)

		headers = append(headers, *hdr)
	}

	return headers
}

// readGzipHeader reads the gzip header metadata from an archive.
func readGzipHeader(t *testing.T, ctx *testctx.TestCtx, path string) gzip.Header {
	t.Helper()

	data, err := fileutils.ReadFile(ctx.FS(), path)
	require.NoError(t, err)

	gzReader, err := gzip.NewReader(bytes.NewReader(data))
	require.NoError(t, err)

	defer gzReader.Close()

	return gzReader.Header
}

func TestNormalizeTarGz(t *testing.T) {
	epoch := time.Unix(0, 0).UTC()

	t.Run("SortsEntriesLexicographically", func(t *testing.T) {
		ctx := testctx.NewCtx()

		createTestTarGz(t, ctx, testArchivePath, []testEntry{
			{name: "z-file.txt", mode: 0o644, typeflag: tar.TypeReg, data: "z content"},
			{name: "a-dir/", mode: 0o755, typeflag: tar.TypeDir},
			{name: "m-file.txt", mode: 0o644, typeflag: tar.TypeReg, data: "m content"},
			{name: "a-dir/b-file.txt", mode: 0o644, typeflag: tar.TypeReg, data: "b content"},
		})

		require.NoError(t, tarutil.NormalizeTarGz(ctx.FS(), testArchivePath, epoch))

		headers := readTarGzEntries(t, ctx)
		require.Len(t, headers, 4)

		assert.Equal(t, "a-dir/", headers[0].Name)
		assert.Equal(t, "a-dir/b-file.txt", headers[1].Name)
		assert.Equal(t, "m-file.txt", headers[2].Name)
		assert.Equal(t, "z-file.txt", headers[3].Name)
	})

	t.Run("NormalizesTimestamps", func(t *testing.T) {
		ctx := testctx.NewCtx()

		createTestTarGz(t, ctx, testArchivePath, []testEntry{
			{
				name:     "file.txt",
				mode:     0o644,
				modTime:  time.Date(2025, 3, 15, 10, 30, 0, 0, time.UTC),
				uid:      1000,
				gid:      1000,
				uname:    "builder",
				gname:    "builders",
				typeflag: tar.TypeReg,
				data:     "hello",
			},
		})

		require.NoError(t, tarutil.NormalizeTarGz(ctx.FS(), testArchivePath, epoch))

		headers := readTarGzEntries(t, ctx)
		require.Len(t, headers, 1)

		assert.True(t, headers[0].ModTime.Equal(epoch), "ModTime should equal epoch")
		assert.True(t, headers[0].AccessTime.IsZero())
		assert.True(t, headers[0].ChangeTime.IsZero())
	})

	t.Run("NormalizesOwnership", func(t *testing.T) {
		ctx := testctx.NewCtx()

		createTestTarGz(t, ctx, testArchivePath, []testEntry{
			{
				name:     "file.txt",
				mode:     0o644,
				uid:      1000,
				gid:      1000,
				uname:    "developer",
				gname:    "devs",
				typeflag: tar.TypeReg,
				data:     "content",
			},
		})

		require.NoError(t, tarutil.NormalizeTarGz(ctx.FS(), testArchivePath, epoch))

		headers := readTarGzEntries(t, ctx)
		require.Len(t, headers, 1)

		assert.Equal(t, 0, headers[0].Uid)
		assert.Equal(t, 0, headers[0].Gid)
		assert.Equal(t, "root", headers[0].Uname)
		assert.Equal(t, "root", headers[0].Gname)
	})

	t.Run("NormalizesPermissions", func(t *testing.T) {
		ctx := testctx.NewCtx()

		createTestTarGz(t, ctx, testArchivePath, []testEntry{
			{name: "dir/", mode: 0o700, typeflag: tar.TypeDir},
			{name: "script.sh", mode: 0o777, typeflag: tar.TypeReg, data: "#!/bin/bash"},
			{name: "data.txt", mode: 0o600, typeflag: tar.TypeReg, data: "secret"},
			{name: "tool", mode: 0o755, typeflag: tar.TypeReg, data: "binary"},
		})

		require.NoError(t, tarutil.NormalizeTarGz(ctx.FS(), testArchivePath, epoch))

		headers := readTarGzEntries(t, ctx)
		require.Len(t, headers, 4)

		assert.Equal(t, int64(fileperms.PublicFile), headers[0].Mode, "non-executable file -> 0644")
		assert.Equal(t, int64(fileperms.PublicDir), headers[1].Mode, "directory -> 0755")
		assert.Equal(t, int64(fileperms.PublicExecutable), headers[2].Mode, "executable script -> 0755")
		assert.Equal(t, int64(fileperms.PublicExecutable), headers[3].Mode, "executable tool -> 0755")
	})

	t.Run("NormalizesGzipHeader", func(t *testing.T) {
		ctx := testctx.NewCtx()

		createTestTarGz(t, ctx, testArchivePath, []testEntry{
			{name: "file.txt", mode: 0o644, typeflag: tar.TypeReg, data: "content"},
		})

		require.NoError(t, tarutil.NormalizeTarGz(ctx.FS(), testArchivePath, epoch))

		gzHdr := readGzipHeader(t, ctx, testArchivePath)
		assert.Empty(t, gzHdr.Name)
		assert.Empty(t, gzHdr.Comment)
		assert.Nil(t, gzHdr.Extra)
		assert.Equal(t, byte(0xFF), gzHdr.OS)
		assert.True(t, gzHdr.ModTime.Equal(epoch) || gzHdr.ModTime.IsZero(),
			"gzip ModTime should be epoch or zero (gzip stores uint32 seconds)")
	})

	t.Run("ProducesDeterministicOutput", func(t *testing.T) {
		// Create two archives with different ordering and metadata, normalize
		// both, and verify they produce identical bytes.
		ctx := testctx.NewCtx()
		path1 := "/test/archive1.tar.gz"
		path2 := "/test/archive2.tar.gz"

		ts1 := time.Date(2024, 1, 1, 0, 0, 0, 0, time.UTC)
		ts2 := time.Date(2025, 6, 15, 12, 0, 0, 0, time.UTC)

		createTestTarGz(t, ctx, path1, []testEntry{
			{name: "b.txt", mode: 0o644, uid: 1000, uname: "alice", modTime: ts1, typeflag: tar.TypeReg, data: "b"},
			{name: "a.txt", mode: 0o600, uid: 2000, uname: "bob", modTime: ts2, typeflag: tar.TypeReg, data: "a"},
		})

		createTestTarGz(t, ctx, path2, []testEntry{
			{name: "a.txt", mode: 0o666, uid: 3000, uname: "charlie", modTime: ts2, typeflag: tar.TypeReg, data: "a"},
			{name: "b.txt", mode: 0o640, uid: 4000, uname: "dave", modTime: ts1, typeflag: tar.TypeReg, data: "b"},
		})

		require.NoError(t, tarutil.NormalizeTarGz(ctx.FS(), path1, epoch))
		require.NoError(t, tarutil.NormalizeTarGz(ctx.FS(), path2, epoch))

		data1, err := fileutils.ReadFile(ctx.FS(), path1)
		require.NoError(t, err)

		data2, err := fileutils.ReadFile(ctx.FS(), path2)
		require.NoError(t, err)

		assert.Equal(t, data1, data2, "archives with same file content should be byte-identical after normalization")
	})

	t.Run("PreservesFileContent", func(t *testing.T) {
		ctx := testctx.NewCtx()

		createTestTarGz(t, ctx, testArchivePath, []testEntry{
			{name: "hello.txt", mode: 0o644, typeflag: tar.TypeReg, data: "hello world"},
		})

		require.NoError(t, tarutil.NormalizeTarGz(ctx.FS(), testArchivePath, epoch))

		// Read back and verify content is preserved.
		file, err := ctx.FS().Open(testArchivePath)
		require.NoError(t, err)

		defer file.Close()

		gzReader, err := gzip.NewReader(file)
		require.NoError(t, err)

		defer gzReader.Close()

		tarReader := tar.NewReader(gzReader)
		hdr, err := tarReader.Next()
		require.NoError(t, err)

		assert.Equal(t, "hello.txt", hdr.Name)

		content, err := io.ReadAll(tarReader)
		require.NoError(t, err)

		assert.Equal(t, "hello world", string(content))
	})

	t.Run("HandlesSymlinks", func(t *testing.T) {
		ctx := testctx.NewCtx()

		var buf bytes.Buffer

		gzWriter := gzip.NewWriter(&buf)
		tarWriter := tar.NewWriter(gzWriter)

		require.NoError(t, tarWriter.WriteHeader(&tar.Header{
			Name:     "link",
			Typeflag: tar.TypeSymlink,
			Linkname: "target.txt",
			Mode:     0o777,
		}))

		require.NoError(t, tarWriter.Close())
		require.NoError(t, gzWriter.Close())
		require.NoError(t, fileutils.WriteFile(ctx.FS(), testArchivePath, buf.Bytes(), fileperms.PublicFile))

		require.NoError(t, tarutil.NormalizeTarGz(ctx.FS(), testArchivePath, epoch))

		headers := readTarGzEntries(t, ctx)
		require.Len(t, headers, 1)

		assert.Equal(t, tar.TypeSymlink, rune(headers[0].Typeflag))
		assert.Equal(t, "target.txt", headers[0].Linkname)
		assert.Equal(t, int64(fileperms.PublicExecutable), headers[0].Mode, "symlink has execute bit -> 0755")
	})

	t.Run("ErrorOnMissingFile", func(t *testing.T) {
		ctx := testctx.NewCtx()

		err := tarutil.NormalizeTarGz(ctx.FS(), "/nonexistent.tar.gz", epoch)
		require.Error(t, err)
		assert.Contains(t, err.Error(), "failed to read tar archive")
	})

	t.Run("ErrorOnInvalidGzip", func(t *testing.T) {
		ctx := testctx.NewCtx()

		require.NoError(t, fileutils.WriteFile(ctx.FS(), testArchivePath, []byte("not gzip"), fileperms.PublicFile))

		err := tarutil.NormalizeTarGz(ctx.FS(), testArchivePath, epoch)
		require.Error(t, err)
		assert.Contains(t, err.Error(), "failed to read tar archive")
	})

	t.Run("EmptyArchive", func(t *testing.T) {
		ctx := testctx.NewCtx()

		createTestTarGz(t, ctx, testArchivePath, nil)

		require.NoError(t, tarutil.NormalizeTarGz(ctx.FS(), testArchivePath, epoch))

		headers := readTarGzEntries(t, ctx)
		assert.Empty(t, headers)
	})
}
