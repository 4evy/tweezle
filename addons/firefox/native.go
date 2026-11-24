package firefox

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"maps"
	"os"
	"path/filepath"
	"strings"

	foundation "github.com/4evy/tweezle/addons/foundation/firefox"
	fileutil "github.com/4evy/tweezle/internal/file"
)

func ReadPreferences(directory string) (map[string]any, error) {
	return foundation.ReadPreferences(directory)
}

func MergeJSONPreference(profile Profile, name string, patch any) (string, error) {
	return foundation.MergeJSONPreference(profile, name, patch)
}

func readPreferenceCalls(data []byte, name string, result map[string]any) error {
	return foundation.ReadPreferenceCalls(data, name, result)
}

func (module Module) applyProfile(ctx context.Context, directory string, preferences map[string]any) error {
	return module.applyProfileWithCommand(ctx, directory, preferences, nil)
}

func (module Module) applyProfileWithCommand(ctx context.Context, directory string, preferences map[string]any, command []string) error {
	if len(module.Config.Providers) == 0 && len(module.Config.Sites) == 0 {
		return ApplyPreferences(ctx, directory, preferences)
	}
	if err := module.Config.validateProviders(); err != nil {
		return err
	}
	if err := validatePreferences(preferences); err != nil {
		return err
	}
	if directory == "" {
		return errors.New("firefox profile directory is required")
	}
	var err error
	directory, err = filepath.Abs(directory)
	if err != nil {
		return err
	}
	directory, err = filepath.EvalSymlinks(directory)
	if err != nil {
		return err
	}
	info, err := os.Stat(directory)
	if err != nil {
		return err
	}
	if !info.IsDir() {
		return errors.New("firefox profile must be an existing directory")
	}
	unlock, err := foundation.LockProfile(directory)
	if err != nil {
		return err
	}
	defer unlock()
	sitePaths := siteDataPaths(module.Config.Sites)
	files, err := module.prepareProfileFiles(ctx, directory, preferences, sitePaths)
	if err != nil {
		return err
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	if err := module.applySites(ctx, directory, sitePaths, command); err != nil {
		return err
	}
	for _, file := range files {
		if err := os.MkdirAll(filepath.Dir(file.Path), fileutil.PrivateDirPerm); err != nil {
			return err
		}
		if err := fileutil.WriteFile(file.Path, file.Data, fileutil.PrivateFilePerm); err != nil {
			return err
		}
	}
	return nil
}

// Prepare every output while the profile is reserved, without writing it
func (module Module) prepareProfileFiles(ctx context.Context, directory string, preferences map[string]any, sitePaths []string) ([]ProfileFile, error) {
	userJS := filepath.Join(directory, "user.js")
	current, err := readProfileFile(userJS)
	if err != nil {
		return nil, err
	}
	merged := preferences
	var files []ProfileFile
	if len(module.Config.Providers) != 0 {
		existing := map[string]any{}
		prefs, err := readProfileFile(filepath.Join(directory, "prefs.js"))
		if err != nil {
			return nil, err
		}
		if err := readPreferenceCalls(prefs, "prefs.js", existing); err != nil {
			return nil, err
		}
		if err := readPreferenceCalls(current, "user.js", existing); err != nil {
			return nil, err
		}
		merged = make(map[string]any, len(preferences))
		maps.Copy(merged, preferences)
		for _, provider := range module.Config.Providers {
			contribution, err := provider.Prepare(Profile{Context: ctx, Directory: directory, Preferences: existing})
			if err != nil {
				return nil, fmt.Errorf("%s profile: %w", provider.Variant(), err)
			}
			for name, value := range contribution.Preferences {
				if _, present := merged[name]; present {
					return nil, fmt.Errorf("preference %q is configured in both firefox.preferences/runtime input and %s", name, provider.Variant())
				}
				merged[name] = value
			}
			files = append(files, contribution.Files...)
		}
	}
	updated, err := foundation.ReplacePreferences(current, merged)
	if err != nil {
		return nil, err
	}
	changed, err := preflightProviderFiles(files, directory, userJS, sitePaths)
	if err != nil {
		return nil, err
	}
	if !bytes.Equal(current, updated) {
		changed = append(changed, ProfileFile{Path: userJS, Data: updated})
	}
	return changed, nil
}

func preflightProviderFiles(files []ProfileFile, directory, userJS string, sitePaths []string) ([]ProfileFile, error) {
	if len(files) == 0 {
		return nil, nil
	}
	reserved := []string{userJS, filepath.Join(directory, ".parentlock"),
		filepath.Join(directory, "parent.lock"), filepath.Join(directory, "lock")}
	for _, path := range sitePaths {
		reserved = append(reserved, filepath.Join(directory, path))
	}
	changed := files[:0]
	for _, file := range files {
		path, err := resolveProfileOutput(file.Path)
		if err != nil {
			return nil, err
		}
		for _, other := range reserved {
			if path == other || strings.HasPrefix(path, other+string(filepath.Separator)) ||
				strings.HasPrefix(other, path+string(filepath.Separator)) {
				return nil, fmt.Errorf("provider output %s conflicts with profile path %s", path, other)
			}
		}
		reserved = append(reserved, path)
		previous, err := readProfileFile(path)
		if err != nil {
			return nil, err
		}
		if !bytes.Equal(previous, file.Data) {
			changed = append(changed, ProfileFile{Path: path, Data: file.Data})
		}
	}
	return changed, nil
}

func readProfileFile(path string) ([]byte, error) {
	info, err := os.Lstat(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	if !info.Mode().IsRegular() {
		return nil, fmt.Errorf("expected a regular Firefox profile file: %s", path)
	}
	return os.ReadFile(path)
}

// Resolve existing directory aliases before comparing output ownership paths
func resolveProfileOutput(path string) (string, error) {
	if !filepath.IsAbs(path) {
		return "", fmt.Errorf("provider output path must be absolute: %s", path)
	}
	path = filepath.Clean(path)
	for parent := filepath.Dir(path); ; parent = filepath.Dir(parent) {
		info, err := os.Lstat(parent)
		if errors.Is(err, os.ErrNotExist) && parent != filepath.Dir(parent) {
			continue
		}
		if err != nil {
			return "", err
		}
		resolved, err := filepath.EvalSymlinks(parent)
		if err != nil {
			return "", err
		}
		if info.Mode()&os.ModeSymlink != 0 {
			info, err = os.Stat(resolved)
			if err != nil {
				return "", err
			}
		}
		if !info.IsDir() {
			return "", fmt.Errorf("provider output parent must be a directory: %s", parent)
		}
		relative, err := filepath.Rel(parent, path)
		if err != nil {
			return "", err
		}
		return filepath.Join(resolved, relative), nil
	}
}
