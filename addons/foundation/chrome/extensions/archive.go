package extensions

import (
	"archive/zip"
	"cmp"
	"errors"
	"fmt"
	"io"
	"os"
	"path"
	"path/filepath"
	"strings"

	fileutil "github.com/4evy/tweezle/internal/file"
	"golang.org/x/text/unicode/norm"
)

const (
	maxArtifactBytes = 128 << 20
	maxExpandedBytes = 512 << 20
	maxZIPIndexBytes = 8 << 20
	maxArchivePaths  = 32 * 1024
	maxArchivePath   = 4096
	maxManifestBytes = 4 << 20
)

// ExtractZipFile extracts regular files and directories without following
// archive-provided symlinks or replacing existing files. os.Root keeps all
// filesystem accesses beneath destination.
func ExtractZipFile(zipPath, destination string) (err error) {
	file, size, err := openBoundedFile(zipPath, maxArtifactBytes)
	if err != nil {
		return err
	}
	defer func() { err = errors.Join(err, file.Close()) }()
	archive, err := openZIP(file, size)
	if err != nil {
		return err
	}
	root, err := os.OpenRoot(destination)
	if err != nil {
		return err
	}
	defer func() { err = errors.Join(err, root.Close()) }()
	return visitArchiveEntries(archive, func(entry *zip.File, name string) error {
		return extractZipEntry(root, entry, filepath.FromSlash(name))
	})
}

// NewReader parses the central directory eagerly. Limit bytes read during
// indexing separately from the total artifact and decompression limits.
func openZIP(reader io.ReaderAt, size int64) (*zip.Reader, error) {
	index := &zipIndexReader{reader: reader, remaining: maxZIPIndexBytes}
	archive, err := zip.NewReader(index, size)
	if err != nil {
		return nil, err
	}
	index.remaining = -1
	return archive, nil
}

type zipIndexReader struct {
	reader    io.ReaderAt
	remaining int64
}

func (index *zipIndexReader) ReadAt(data []byte, offset int64) (int, error) {
	if index.remaining < 0 {
		return index.reader.ReadAt(data, offset)
	}
	if int64(len(data)) > index.remaining {
		return 0, fmt.Errorf("ZIP index exceeds %d bytes", maxZIPIndexBytes)
	}
	n, err := index.reader.ReadAt(data, offset)
	index.remaining -= int64(n)
	return n, err
}

type archivePath struct {
	name      string
	directory bool
	explicit  bool
}

func visitArchiveEntries(archive *zip.Reader, visit func(*zip.File, string) error) error {
	if len(archive.File) > maxArchivePaths {
		return fmt.Errorf("ZIP archive exceeds %d entries", maxArchivePaths)
	}
	paths := make(map[string]archivePath, len(archive.File))
	var expanded uint64
	for _, entry := range archive.File {
		if len(entry.Name) > maxArchivePath {
			return fmt.Errorf("ZIP archive path exceeds %d bytes", maxArchivePath)
		}
		name := path.Clean(strings.ReplaceAll(entry.Name, "\\", "/"))
		mode := entry.Mode()
		if name == "." {
			if mode.IsDir() {
				continue
			}
			return fmt.Errorf("invalid ZIP entry name %q", entry.Name)
		}
		if _, err := filepath.Localize(name); err != nil {
			return fmt.Errorf("archive entry escapes destination: %s", entry.Name)
		}
		if err := addArchivePath(paths, name, mode.IsDir()); err != nil {
			return err
		}
		if mode.IsRegular() {
			if entry.UncompressedSize64 > maxExpandedBytes-expanded {
				return fmt.Errorf("ZIP archive exceeds %d expanded bytes", maxExpandedBytes)
			}
			expanded += entry.UncompressedSize64
		}
		if err := visit(entry, name); err != nil {
			return err
		}
	}
	return nil
}

func addArchivePath(paths map[string]archivePath, name string, directory bool) error {
	// Include implicit parents so one file cannot silently replace a directory
	// and case or normalization aliases cannot merge on portable filesystems
	for current, explicit := name, true; current != "."; current, explicit = path.Dir(current), false {
		key := norm.NFC.String(strings.ToLower(current))
		previous, exists := paths[key]
		if exists {
			if previous.name != current || !previous.directory || !directory || (previous.explicit && explicit) {
				return fmt.Errorf("ZIP archive has colliding paths %q and %q", previous.name, current)
			}
			if explicit {
				previous.explicit = true
				paths[key] = previous
			}
			// A recorded directory already has validated ancestors
			return nil
		}
		if len(paths) >= maxArchivePaths {
			return fmt.Errorf("ZIP archive exceeds %d paths", maxArchivePaths)
		}
		paths[key] = archivePath{name: current, directory: directory, explicit: explicit}
		directory = true
	}
	return nil
}

func extractZipEntry(root *os.Root, entry *zip.File, target string) (err error) {
	mode := entry.Mode()
	if mode.IsDir() {
		return root.MkdirAll(target, fileutil.DefaultDirPerm)
	}
	if !mode.IsRegular() {
		return nil
	}
	source, err := entry.Open()
	if err != nil {
		return err
	}
	defer func() { err = errors.Join(err, source.Close()) }()
	if err := root.MkdirAll(filepath.Dir(target), fileutil.DefaultDirPerm); err != nil {
		return err
	}
	destination, err := root.OpenFile(
		target,
		os.O_WRONLY|os.O_CREATE|os.O_EXCL,
		cmp.Or(mode.Perm(), fileutil.DefaultFilePerm),
	)
	if err != nil {
		return err
	}
	want := int64(entry.UncompressedSize64)
	n, copyErr := io.Copy(destination, io.LimitReader(source, want+1))
	if copyErr == nil && n != want {
		copyErr = fmt.Errorf("archive entry %q does not match its declared size", entry.Name)
	}
	return errors.Join(copyErr, destination.Close())
}

func boundedFileSize(path string, limit int64) (int64, error) {
	info, err := os.Lstat(path)
	if err != nil {
		return 0, err
	}
	if !info.Mode().IsRegular() {
		return 0, fmt.Errorf("%s is not a regular file", path)
	}
	if info.Size() > limit {
		return 0, fmt.Errorf("%s exceeds %d bytes", path, limit)
	}
	return info.Size(), nil
}

func openBoundedFile(path string, limit int64) (*os.File, int64, error) {
	size, err := boundedFileSize(path, limit)
	if err != nil {
		return nil, 0, err
	}
	file, err := os.Open(path)
	return file, size, err
}

func readUnpackedManifest(path string) (_ []byte, err error) {
	file, _, err := openBoundedFile(path, maxManifestBytes)
	if err != nil {
		return nil, err
	}
	defer func() { err = errors.Join(err, file.Close()) }()
	data, err := io.ReadAll(io.LimitReader(file, maxManifestBytes+1))
	if err != nil {
		return nil, err
	}
	if len(data) > maxManifestBytes {
		return nil, fmt.Errorf("%s exceeds %d bytes", path, maxManifestBytes)
	}
	return data, nil
}
