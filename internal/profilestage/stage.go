// Package profilestage isolates native browser writes and installs selected
// persistent stores only after the browser exits successfully
package profilestage

import (
	"crypto/sha256"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"syscall"

	fileutil "github.com/4evy/tweezle/internal/file"
)

type Stage struct {
	profile     string
	root        *os.Root
	temporary   string
	directory   string
	paths       []string
	before      [][32]byte
	directories bool
	keep        bool
}

func New(profile string, paths []string) (*Stage, error) {
	return newStage(profile, paths, "profile", false)
}

// NewDirectoryStores stages stores which must be directories when present
func NewDirectoryStores(profile string, paths []string, directory string) (*Stage, error) {
	return newStage(profile, paths, directory, true)
}

func newStage(profile string, paths []string, directory string, directories bool) (*Stage, error) {
	if profile == "" {
		return nil, errors.New("profile directory is required")
	}
	profile, err := filepath.Abs(profile)
	if err != nil {
		return nil, err
	}
	if directory == "." || filepath.Base(directory) != directory || !filepath.IsLocal(directory) {
		return nil, fmt.Errorf("invalid staged profile directory %q", directory)
	}
	for i, path := range paths {
		if !filepath.IsLocal(path) || path == "." || filepath.Clean(path) != path {
			return nil, fmt.Errorf("invalid storage path %q", path)
		}
		for _, other := range paths[:i] {
			if path == other || strings.HasPrefix(path, other+string(filepath.Separator)) || strings.HasPrefix(other, path+string(filepath.Separator)) {
				return nil, fmt.Errorf("overlapping storage paths %q and %q", path, other)
			}
		}
	}
	if err := os.MkdirAll(profile, fileutil.PrivateDirPerm); err != nil {
		return nil, err
	}
	resolved, err := filepath.EvalSymlinks(profile)
	if err != nil {
		return nil, err
	}
	root, err := os.OpenRoot(resolved)
	if err != nil {
		return nil, err
	}
	stage := &Stage{
		profile: profile, root: root, directory: directory,
		paths: slices.Clone(paths), before: make([][32]byte, len(paths)),
		directories: directories,
	}
	// Stage and rollback renames must remain on the profile's filesystem
	temporary, err := os.MkdirTemp(resolved, ".tweezle-stage-")
	if err != nil {
		stage.Remove()
		return nil, err
	}
	stage.temporary = filepath.Base(temporary)
	if err := root.Mkdir(stage.staged("."), fileutil.PrivateDirPerm); err != nil {
		stage.Remove()
		return nil, err
	}
	for i, relative := range paths {
		if err := checkParents(root, relative); err != nil {
			stage.Remove()
			return nil, err
		}
		before, err := storageFingerprint(root, relative, stage.staged(relative), directories)
		if err == nil {
			var after [32]byte
			after, err = storageFingerprint(root, relative, "", directories)
			if err == nil && before != after {
				err = errors.New("profile storage changed while staging; close the browser and retry")
			}
		}
		if err != nil {
			stage.Remove()
			return nil, fmt.Errorf("stage %s: %w", relative, err)
		}
		stage.before[i] = before
	}
	return stage, nil
}

func (stage *Stage) Root() string { return filepath.Join(stage.root.Name(), stage.temporary) }

func (stage *Stage) Directory() string { return filepath.Join(stage.Root(), stage.directory) }

func (stage *Stage) staged(relative string) string {
	return filepath.Join(stage.temporary, stage.directory, relative)
}

func (stage *Stage) Remove() {
	if !stage.keep && stage.temporary != "" {
		_ = stage.root.RemoveAll(stage.temporary)
	}
	_ = stage.root.Close()
}

func checkParents(root *os.Root, relative string) error {
	var path string
	for part := range strings.SplitSeq(relative, string(filepath.Separator)) {
		path = filepath.Join(path, part)
		info, err := root.Lstat(path)
		if errors.Is(err, os.ErrNotExist) {
			return nil
		}
		if err != nil {
			return err
		}
		if info.Mode()&os.ModeSymlink != 0 {
			return fmt.Errorf("storage path must not contain symlinks: %s", path)
		}
	}
	return nil
}

func storageFingerprint(root *os.Root, source, destination string, directories bool) ([32]byte, error) {
	var digest [32]byte
	info, err := root.Lstat(source)
	if errors.Is(err, os.ErrNotExist) {
		return digest, nil
	}
	if err != nil {
		return digest, err
	}
	if directories && !info.IsDir() {
		return digest, fmt.Errorf("storage path is not a directory: %s", source)
	}
	hash := sha256.New()
	type directoryMode struct {
		path string
		mode os.FileMode
	}
	var copiedDirectories []directoryMode
	if destination != "" {
		if err := root.MkdirAll(filepath.Dir(destination), fileutil.PrivateDirPerm); err != nil {
			return digest, err
		}
	}
	sourceName := filepath.ToSlash(source)
	err = fs.WalkDir(root.FS(), sourceName, func(name string, entry fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		entryInfo, err := entry.Info()
		if err != nil {
			return err
		}
		var relative string
		if name == sourceName {
			if !os.SameFile(entryInfo, info) {
				return fmt.Errorf("storage path changed while reading: %s", source)
			}
		} else {
			relative = name[len(sourceName)+1:]
		}
		mode := entryInfo.Mode()
		_, _ = fmt.Fprintf(hash, "%s\x00%d\x00", relative, mode)
		var target string
		if destination != "" {
			target = filepath.Join(destination, filepath.FromSlash(relative))
		}
		if entryInfo.IsDir() {
			if target != "" {
				if err := root.Mkdir(target, fileutil.PrivateDirPerm); err != nil {
					return err
				}
				copiedDirectories = append(copiedDirectories, directoryMode{target, mode})
			}
			return nil
		}
		if !mode.IsRegular() {
			return fmt.Errorf("unsupported storage entry: %s", name)
		}
		_, _ = fmt.Fprintf(hash, "%d\x00", entryInfo.Size())
		return copyStorageFile(root, filepath.FromSlash(name), target, entryInfo, hash)
	})
	if err != nil {
		return digest, err
	}
	for _, item := range slices.Backward(copiedDirectories) {
		directory, err := root.OpenFile(item.path, os.O_RDONLY|syscall.O_NOFOLLOW|syscall.O_DIRECTORY, 0)
		if err != nil {
			return digest, err
		}
		if err := errors.Join(directory.Chmod(item.mode), directory.Close()); err != nil {
			return digest, err
		}
	}
	hash.Sum(digest[:0])
	return digest, nil
}

func copyStorageFile(root *os.Root, source, target string, info fs.FileInfo, hash io.Writer) (err error) {
	file, err := root.OpenFile(source, os.O_RDONLY|syscall.O_NOFOLLOW|syscall.O_NONBLOCK, 0)
	if err != nil {
		return err
	}
	defer func() { err = errors.Join(err, file.Close()) }()
	opened, err := file.Stat()
	if err != nil {
		return err
	}
	if !os.SameFile(info, opened) || opened.Mode() != info.Mode() || opened.Size() != info.Size() {
		return fmt.Errorf("storage file changed while reading: %s", source)
	}
	if target == "" {
		_, err = io.Copy(hash, file)
		return err
	}
	copied, err := root.OpenFile(target, os.O_CREATE|os.O_EXCL|os.O_WRONLY, fileutil.PrivateFilePerm)
	if err != nil {
		return err
	}
	defer func() { err = errors.Join(err, copied.Close()) }()
	if _, err := io.Copy(io.MultiWriter(copied, hash), file); err != nil {
		return err
	}
	return copied.Chmod(info.Mode())
}

// Commit checks all original and staged stores before replacing any of them
// A failed rename restores every earlier store, retaining recovery data if
// restoration itself fails. Callers must keep the browser stopped throughout
func (stage *Stage) Commit(check func() error) error {
	if check == nil {
		return errors.New("profile lock check is required")
	}
	profileBefore, err := stage.root.Stat(".")
	if err != nil {
		return err
	}
	profileNow, err := os.Stat(stage.profile)
	if err != nil {
		return err
	}
	if !os.SameFile(profileBefore, profileNow) {
		return errors.New("browser profile moved during configuration")
	}
	changed := make([]bool, len(stage.paths))
	configuredPaths := make([]bool, len(stage.paths))
	for i, relative := range stage.paths {
		if err := checkParents(stage.root, relative); err != nil {
			return err
		}
		current, err := storageFingerprint(stage.root, relative, "", stage.directories)
		if err != nil {
			return err
		}
		if current != stage.before[i] {
			return errors.New("profile storage changed during configuration; close the browser and retry")
		}
		if err := checkParents(stage.root, stage.staged(relative)); err != nil {
			return err
		}
		configured, err := storageFingerprint(stage.root, stage.staged(relative), "", stage.directories)
		if err != nil {
			return err
		}
		changed[i] = current != configured
		configuredPaths[i] = configured != ([32]byte{})
	}
	if err := check(); err != nil {
		return err
	}
	type replacement struct {
		target, backup, staged string
		previous, installed    bool
	}
	var replacements []replacement
	rollback := func(cause error) error {
		for _, item := range slices.Backward(replacements) {
			if item.installed {
				if err := stage.root.Rename(item.target, item.staged); err != nil {
					stage.keep = true
					cause = errors.Join(cause, err)
					continue
				}
			}
			if item.previous {
				if err := stage.root.Rename(item.backup, item.target); err != nil {
					stage.keep = true
					cause = errors.Join(cause, err)
				}
			}
		}
		if stage.keep {
			cause = errors.Join(cause, fmt.Errorf("previous storage retained for recovery at %s", stage.Root()))
		}
		return cause
	}
	for i, relative := range stage.paths {
		if !changed[i] {
			continue
		}
		if err := stage.root.MkdirAll(filepath.Dir(relative), fileutil.PrivateDirPerm); err != nil {
			return rollback(err)
		}
		replacements = append(replacements, replacement{
			target: relative, staged: stage.staged(relative),
			backup: filepath.Join(stage.temporary, fmt.Sprintf("previous-%d", i)),
		})
		item := &replacements[len(replacements)-1]
		if stage.before[i] != ([32]byte{}) {
			if err := stage.root.Rename(relative, item.backup); err != nil {
				return rollback(err)
			}
			item.previous = true
		}
		if configuredPaths[i] {
			if err := stage.root.Rename(item.staged, relative); err != nil {
				return rollback(err)
			}
			item.installed = true
		}
	}
	return nil
}
