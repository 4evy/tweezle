package chromium

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"time"

	foundation "github.com/4evy/tweezle/addons/foundation/chrome"
	"github.com/4evy/tweezle/addons/foundation/sites"
	"github.com/4evy/tweezle/internal/profilestage"
	xdgdirs "github.com/4evy/tweezle/internal/xdg"
)

// These are persistent native storage paths, not profile preferences or session
// tabs. Keep legacy and bucket-based stores together with their quota metadata
// chromium/chromium@583bbf02cefc4bb6abd92f546d4d872f83725193:
// content/browser/indexed_db/indexed_db_context_impl.cc, GetDataPath
// content/browser/cache_storage/cache_storage_manager.cc, ConstructBucketPath
// chrome/browser/net/profile_network_context_service.cc, ConfigureNetworkContextParams
var siteCookieStoragePaths = []string{
	"Cookies", "Cookies-journal", "Cookies-wal", "Cookies-shm",
	"Network/Cookies", "Network/Cookies-journal", "Network/Cookies-wal", "Network/Cookies-shm",
}

var siteQuotaStoragePaths = []string{
	"IndexedDB", "Service Worker/CacheStorage", "WebStorage",
	"QuotaManager", "QuotaManager-journal", "QuotaManager-wal", "QuotaManager-shm",
}

var siteLocalStoragePaths = []string{
	"Local Storage",
	"LocalStorage", "LocalStorage-journal", "LocalStorage-wal", "LocalStorage-shm",
}

func siteStoragePaths(configs []sites.Site) []string {
	var cookies, local, quota []string
	for _, config := range configs {
		if config.Script != "" || config.ClearCookies || len(config.Cookies) > 0 {
			cookies = siteCookieStoragePaths
		}
		if config.Script != "" || config.ClearLocalStorage || len(config.LocalStorage) > 0 || len(config.LocalStorageRemove) > 0 {
			local = siteLocalStoragePaths
		}
		if config.Script != "" || len(config.IndexedDB) > 0 {
			quota = siteQuotaStoragePaths
		}
	}
	return slices.Concat(cookies, local, quota)
}

func (browser Browser) applySites(ctx context.Context, options ApplyOptions, entries []foundation.PreparedSite) error {
	if len(entries) == 0 {
		return nil
	}
	if err := browser.ensureProfileNotRunning(options.ProfileDir); err != nil {
		return err
	}
	paths := siteStoragePaths(browser.Config.Sites)
	stage, err := profilestage.New(options.ProfileDir, paths)
	if err != nil {
		return err
	}
	defer stage.Remove()
	root := stage.Root()
	var originalState *os.Root
	var state []byte
	var stateExists bool
	if slices.Contains(paths, "Cookies") {
		// Copy the user-data encryption state, never expose the original path
		// to the browser or commit its temporary replacement
		originalState, err = os.OpenRoot(filepath.Dir(filepath.Dir(root)))
		if err != nil {
			return fmt.Errorf("open cookie encryption state directory: %w", err)
		}
		defer func() { _ = originalState.Close() }()
		state, err = originalState.ReadFile(LocalStateFilename)
		stateExists = err == nil
		if err != nil && !errors.Is(err, os.ErrNotExist) {
			return fmt.Errorf("read cookie encryption state: %w", err)
		}
		if stateExists {
			if err := os.WriteFile(filepath.Join(root, LocalStateFilename), state, 0o400); err != nil {
				return fmt.Errorf("stage cookie encryption state: %w", err)
			}
		}
	}
	executable := options.BrowserExecutable
	if executable == "" {
		if runtime.GOOS == "darwin" {
			executable = filepath.Join(expandPathTemplate(browser.Config.MacOS.AppDir), browser.Config.MacOS.LauncherPath)
		} else {
			extensionRoot := options.ExtensionRoot
			if extensionRoot == "" {
				extensionRoot = filepath.Join(xdgdirs.Current().DataHome, "tweezle", browser.Config.ExecutableName)
			}
			executable = filepath.Join(browser.linuxAppDir(&InstallOptions{Root: extensionRoot}), browser.Config.Linux.LauncherName)
		}
	}
	// Bound startup and shutdown independently of each site's script deadline
	operationCtx, cancel := context.WithTimeout(ctx, siteOperationTimeout(browser.Config.Sites))
	defer cancel()
	command, err := browser.maintenanceCommand(operationCtx, executable, nil, []string{root}, root)
	if err != nil {
		return err
	}
	if err := foundation.EvaluateSitesCommand(operationCtx, command, root, entries); err != nil {
		return err
	}
	return stage.Commit(func() error {
		if err := operationCtx.Err(); err != nil {
			return err
		}
		if err := browser.ensureProfileNotRunning(options.ProfileDir); err != nil {
			return err
		}
		if originalState == nil {
			return nil
		}
		current, err := originalState.ReadFile(LocalStateFilename)
		if stateExists {
			if err != nil {
				return fmt.Errorf("recheck cookie encryption state: %w", err)
			}
			if !bytes.Equal(state, current) {
				return errors.New("browser Local State changed during site configuration")
			}
		} else if err == nil || !errors.Is(err, os.ErrNotExist) {
			return errors.New("browser Local State changed during site configuration")
		}
		return nil
	})
}

func siteOperationTimeout(configs []sites.Site) time.Duration {
	// Allow a separate browser startup/shutdown window in addition to each
	// site's deadline, saturating rather than overflowing for large configs
	const browserLifecycleTimeout = 180 * time.Second
	const defaultSiteTimeout = 180 * time.Second
	timeout := browserLifecycleTimeout
	for _, config := range configs {
		siteTimeout := defaultSiteTimeout
		if config.TimeoutSeconds > 0 {
			siteTimeout = time.Duration(config.TimeoutSeconds) * time.Second
		}
		if siteTimeout > time.Duration(math.MaxInt64)-timeout {
			return time.Duration(math.MaxInt64)
		}
		timeout += siteTimeout
	}
	return timeout
}
