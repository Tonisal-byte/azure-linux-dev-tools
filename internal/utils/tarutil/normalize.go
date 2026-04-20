// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package tarutil

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"errors"
	"fmt"
	"io"
	"sort"
	"time"

	"github.com/microsoft/azure-linux-dev-tools/internal/global/opctx"
	"github.com/microsoft/azure-linux-dev-tools/internal/utils/fileperms"
	"github.com/microsoft/azure-linux-dev-tools/internal/utils/fileutils"
)

// tarEntry holds a single tar archive entry with its header and data.
type tarEntry struct {
	header *tar.Header
	data   []byte
}

// NormalizeTarGz normalizes a gzip-compressed tar archive for reproducibility.
//
// It performs the following normalizations:
//   - Sorts entries lexicographically by path
//   - Sets all modification times to the given timestamp
//   - Clears access and change times
//   - Sets uid/gid to 0 and owner/group names to "root"
//   - Normalizes permissions (directories: 0755, executable files: 0755, other files: 0644)
//   - Removes non-deterministic PAX extended attributes
//   - Produces deterministic gzip output (fixed OS byte, no embedded filename)
func NormalizeTarGz(fs opctx.FS, path string, timestamp time.Time) error {
	entries, err := readTarGzEntries(fs, path)
	if err != nil {
		return fmt.Errorf("failed to read tar archive %#q:\n%w", path, err)
	}

	sort.Slice(entries, func(i, j int) bool {
		return entries[i].header.Name < entries[j].header.Name
	})

	normalizeHeaders(entries, timestamp)

	data, err := writeTarGzToBytes(entries, timestamp)
	if err != nil {
		return fmt.Errorf("failed to write normalized tar archive %#q:\n%w", path, err)
	}

	// Write to a temp file first, then rename for atomicity.
	tmpPath := path + ".normalized.tmp"

	if err := fileutils.WriteFile(fs, tmpPath, data, fileperms.PublicFile); err != nil {
		return fmt.Errorf("failed to write temporary file %#q:\n%w", tmpPath, err)
	}

	if err := fs.Rename(tmpPath, path); err != nil {
		// Clean up the temp file on rename failure.
		_ = fs.Remove(tmpPath)

		return fmt.Errorf("failed to rename normalized archive to %#q:\n%w", path, err)
	}

	return nil
}

// readTarGzEntries reads all entries from a gzip-compressed tar archive.
func readTarGzEntries(fs opctx.FS, path string) ([]tarEntry, error) {
	file, err := fs.Open(path)
	if err != nil {
		return nil, fmt.Errorf("failed to open file:\n%w", err)
	}
	defer file.Close()

	gzReader, err := gzip.NewReader(file)
	if err != nil {
		return nil, fmt.Errorf("failed to create gzip reader:\n%w", err)
	}
	defer gzReader.Close()

	tarReader := tar.NewReader(gzReader)

	var entries []tarEntry

	for {
		hdr, err := tarReader.Next()
		if errors.Is(err, io.EOF) {
			break
		}

		if err != nil {
			return nil, fmt.Errorf("failed to read tar entry:\n%w", err)
		}

		var data []byte
		if hdr.Size > 0 {
			data, err = io.ReadAll(tarReader)
			if err != nil {
				return nil, fmt.Errorf("failed to read data for entry %#q:\n%w", hdr.Name, err)
			}
		}

		entries = append(entries, tarEntry{header: hdr, data: data})
	}

	return entries, nil
}

// normalizeHeaders normalizes all tar entry headers for reproducibility.
func normalizeHeaders(entries []tarEntry, timestamp time.Time) {
	for _, entry := range entries {
		entry.header.ModTime = timestamp
		entry.header.AccessTime = time.Time{}
		entry.header.ChangeTime = time.Time{}
		entry.header.Uid = 0
		entry.header.Gid = 0
		entry.header.Uname = "root"
		entry.header.Gname = "root"

		// Clear non-deterministic extended attributes. The tar writer will
		// regenerate any necessary PAX records from the normalized header fields.
		entry.header.PAXRecords = nil
		entry.header.Format = tar.FormatUnknown

		switch entry.header.Typeflag {
		case tar.TypeDir:
			entry.header.Mode = int64(fileperms.PublicDir)
		default:
			if entry.header.Mode&0o111 != 0 {
				entry.header.Mode = int64(fileperms.PublicExecutable)
			} else {
				entry.header.Mode = int64(fileperms.PublicFile)
			}
		}
	}
}

// writeTarGzToBytes writes the normalized entries to a gzip-compressed tar
// archive in memory and returns the resulting bytes.
func writeTarGzToBytes(entries []tarEntry, timestamp time.Time) ([]byte, error) {
	var buf bytes.Buffer

	gzWriter, err := gzip.NewWriterLevel(&buf, gzip.BestCompression)
	if err != nil {
		return nil, fmt.Errorf("failed to create gzip writer:\n%w", err)
	}

	// Normalize gzip header for reproducibility.
	gzWriter.ModTime = timestamp
	gzWriter.Name = ""
	gzWriter.Comment = ""
	gzWriter.Extra = nil
	gzWriter.OS = 0xFF // Unknown OS — avoids platform-dependent value.

	tarWriter := tar.NewWriter(gzWriter)

	for _, entry := range entries {
		if err := tarWriter.WriteHeader(entry.header); err != nil {
			return nil, fmt.Errorf("failed to write header for %#q:\n%w", entry.header.Name, err)
		}

		if len(entry.data) > 0 {
			if _, err := tarWriter.Write(entry.data); err != nil {
				return nil, fmt.Errorf("failed to write data for %#q:\n%w", entry.header.Name, err)
			}
		}
	}

	if err := tarWriter.Close(); err != nil {
		return nil, fmt.Errorf("failed to close tar writer:\n%w", err)
	}

	if err := gzWriter.Close(); err != nil {
		return nil, fmt.Errorf("failed to close gzip writer:\n%w", err)
	}

	return buf.Bytes(), nil
}
