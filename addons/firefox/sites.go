package firefox

import (
	"context"
	"fmt"
	"runtime"
	"slices"

	"github.com/4evy/tweezle"
	foundation "github.com/4evy/tweezle/addons/foundation/firefox"
	"github.com/4evy/tweezle/addons/foundation/sites"
	"github.com/4evy/tweezle/internal/profilestage"
)

// Stage quota storage and its migration inputs together. Firefox, not Tweezle,
// owns their schemas and checkpoints SQLite WAL files during graceful shutdown
var siteQuotaDataPaths = []string{
	"indexedDB", "storage",
	"storage.sqlite", "storage.sqlite-wal", "storage.sqlite-shm", "storage.sqlite-journal",
	"webappsstore.sqlite", "webappsstore.sqlite-wal", "webappsstore.sqlite-shm", "webappsstore.sqlite-journal",
}

var siteCookieDataPaths = []string{
	"cookies.sqlite", "cookies.sqlite-wal", "cookies.sqlite-shm", "cookies.sqlite-journal",
}

func siteDataPaths(configs []sites.Site) []string {
	var cookies, quota []string
	for _, config := range configs {
		if config.Script != "" || config.ClearCookies || len(config.Cookies) > 0 {
			cookies = siteCookieDataPaths
		}
		if config.Script != "" || config.ClearLocalStorage || len(config.LocalStorage) > 0 || len(config.LocalStorageRemove) > 0 || len(config.IndexedDB) > 0 {
			quota = siteQuotaDataPaths
		}
	}
	return slices.Concat(cookies, quota)
}

// applySites requires the caller to hold the native reservation on directory
func (module Module) applySites(ctx context.Context, directory string, paths, command []string) error {
	if len(module.Config.Sites) == 0 {
		return nil
	}
	stage, err := profilestage.New(directory, paths)
	if err != nil {
		return err
	}
	defer stage.Remove()
	config := module.Config.defaults()
	if runtime.GOOS == "linux" && config.Linux.Flatpak.AppID != "" {
		application, err := config.Linux.Flatpak.Resolve(ctx)
		if err != nil {
			return err
		}
		command, err = config.Linux.Flatpak.Command(application, nil, []string{stage.Directory()})
		if err != nil {
			return err
		}
	} else if len(command) == 0 {
		var mode tweezle.Mode
		switch runtime.GOOS {
		case "darwin":
			mode = tweezle.ModeMacOS
		case "linux":
			mode = tweezle.ModeLinux
		default:
			return fmt.Errorf("platform %s does not support Firefox site data", runtime.GOOS)
		}
		executable, err := config.browserExecutable(mode, "")
		if err != nil {
			return err
		}
		command = []string{executable}
	}
	if err := foundation.ApplySites(ctx, command, stage.Directory(), config.Sites); err != nil {
		return err
	}
	// LockProfile remains held here, and Commit fingerprints every selected
	// source path again before replacing anything in the original profile
	return stage.Commit(ctx.Err)
}
