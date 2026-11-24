// Package chromiumflags handles Chromium's POSIX switch grammar
package chromiumflags

import (
	"fmt"
	"path/filepath"
	"strings"
)

const (
	userDataDir = "user-data-dir"
	profileDir  = "profile-directory"
)

// Split separates the browser switch region from arguments after --
func Split(args []string) (before, after []string) {
	for i, arg := range args {
		if trim(arg) == "--" {
			return args[:i], args[i:]
		}
	}
	return args, nil
}

// Switch returns the name and value of a POSIX Chromium switch
func Switch(arg string) (name, value string) {
	arg = trim(arg)
	arg, ok := strings.CutPrefix(arg, "-")
	if !ok || arg == "" || arg == "-" {
		return "", ""
	}
	arg = strings.TrimPrefix(arg, "-")
	name, value, _ = strings.Cut(arg, "=")
	return name, value
}

func trim(arg string) string { return strings.Trim(arg, " \t\n\v\f\r") }

// Profile resolves the configured profile directory and its two selectors
func Profile(path string) (absolute string, selectors [2]string, err error) {
	if path == "" {
		return "", selectors, nil
	}
	absolute, err = filepath.Abs(path)
	if err != nil {
		return "", selectors, err
	}
	base, parent := filepath.Base(absolute), filepath.Dir(absolute)
	if trim(base) != base || trim(parent) != parent || strings.ContainsAny(absolute, "\r\n\x00") {
		return "", selectors, fmt.Errorf("profile directory has whitespace or control characters Chromium does not support: %q", absolute)
	}
	if parent == absolute {
		return "", selectors, fmt.Errorf("profile directory cannot be the filesystem root: %s", absolute)
	}
	return absolute, [2]string{"--" + userDataDir + "=" + parent, "--" + profileDir + "=" + base}, nil
}

// CheckProfile rejects selectors that would launch a different profile
func CheckProfile(args []string, profile string) error {
	if profile == "" {
		return nil
	}
	before, _ := Split(args)
	for _, arg := range before {
		name, value := Switch(arg)
		switch name {
		case userDataDir:
			if filepath.IsAbs(value) && filepath.Clean(value) == filepath.Dir(profile) {
				continue
			}
		case profileDir:
			if value == filepath.Base(profile) {
				continue
			}
		default:
			continue
		}
		return fmt.Errorf("switch %q conflicts with profile directory %s", arg, profile)
	}
	return nil
}
