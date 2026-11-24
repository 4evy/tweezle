package chromium

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"slices"

	foundation "github.com/4evy/tweezle/addons/foundation/chrome"
	fileutil "github.com/4evy/tweezle/internal/file"
)

func (browser Browser) maintenanceCommand(ctx context.Context, executable string, readOnly, writable []string, stateDir string) ([]string, error) {
	config := browser.Config.Linux.Flatpak
	if runtime.GOOS != "linux" || config.AppID == "" {
		return []string{executable}, nil
	}
	app, err := config.Resolve(ctx)
	if err != nil {
		return nil, err
	}
	// Some packages inspect or clean XDG_CONFIG_HOME in their wrapper before
	// Chromium sees --user-data-dir
	configHome := filepath.Join(stateDir, ".tweezle-config")
	if err := os.Mkdir(configHome, 0o700); err != nil {
		return nil, err
	}
	command, err := config.Command(app, readOnly, append(slices.Clone(writable), configHome))
	if err != nil {
		return nil, err
	}
	return slices.Insert(command, len(command)-1, "--env=XDG_CONFIG_HOME="+configHome), nil
}

func (browser Browser) readIdentity(ctx context.Context, executable string) (browserIdentity, error) {
	if runtime.GOOS != "linux" || browser.Config.Linux.Flatpak.AppID == "" {
		return foundation.ReadBrowserIdentity(ctx, executable)
	}
	return foundation.ReadBrowserIdentityCommand(ctx, func(profile string) ([]string, error) {
		return browser.maintenanceCommand(ctx, executable, nil, []string{profile}, profile)
	})
}

func (browser Browser) installFlatpak(ctx context.Context, options *InstallOptions) error {
	if options.AppDir != "" {
		return errors.New("--app-dir cannot be used with a Flatpak application")
	}
	if options.Root == "" || options.BinDir == "" || options.LauncherExecutable == "" {
		return errors.New("flatpak installation requires root, bin directory, and launcher executable")
	}
	config := browser.Config.Linux.Flatpak
	app, err := config.Resolve(ctx)
	if err != nil {
		return err
	}
	if err := ensureDirectories(options.Root, options.BinDir); err != nil {
		return err
	}
	paths := browser.Config.Paths[string(ModeLinux)]
	needsExternal := len(browser.Extensions.ChromeStore)+len(browser.Extensions.CRX)+len(browser.Extensions.UpdateURL) > 0
	if len(paths.ExternalExtensionDirs) == 0 {
		directory, err := app.ExtensionDirectory(".Extension", "chromium/extensions")
		if err != nil && needsExternal {
			return err
		}
		if err == nil {
			paths.ExternalExtensionDirs = []string{filepath.Join(directory, "extensions")}
		}
	}
	policyPath := expandPathTemplate(paths.PoliciesFile)
	if policyPath == "" {
		policyPath, err = app.PolicyDestination(".Extension", "chromium/extensions", "policies/managed/tweezle.json", len(browser.ManagedPolicies) > 0)
		if err != nil {
			return err
		}
	}
	browser.Config.Paths = overlayValues(browser.Config.Paths, map[string]ModePaths{string(ModeLinux): paths})
	profile := options.ProfileDir
	var writable []string
	if profile != "" {
		writable = []string{filepath.Dir(profile)}
	}
	options.launcherCommand, err = config.Command(app, []string{options.Root}, writable)
	if err != nil {
		return err
	}
	// The sandbox keeps the package’s original portal and application
	// identity
	options.extraLauncherFlags = slices.Concat(browser.Config.Linux.WrapperFlags, options.extraLauncherFlags)
	if err := browser.configureApp(ctx, options, app.Executable); err != nil {
		return err
	}
	if policyPath != "" {
		var output bytes.Buffer
		if err := EncodeManagedPolicy(&output, browser.ManagedPolicies); err != nil {
			return err
		}
		if _, err := fileutil.WriteTextIfChanged(policyPath, output.String()); err != nil {
			return err
		}
	}
	return app.InstallDesktop(filepath.Join(options.BinDir, browser.Config.ExecutableName), browser.Config.ExecutableName)
}
