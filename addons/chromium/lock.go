package chromium

import (
	"runtime"

	foundation "github.com/4evy/tweezle/addons/foundation/chrome"
)

func EnsureProfileNotRunning(profileDir string) error {
	return foundation.EnsureProfileNotRunning(profileDir)
}

func (browser Browser) ensureProfileNotRunning(profileDir string) error {
	if runtime.GOOS == "linux" && browser.Config.Linux.Flatpak.AppID != "" {
		return foundation.EnsureSandboxProfileNotRunning(profileDir)
	}
	return EnsureProfileNotRunning(profileDir)
}
