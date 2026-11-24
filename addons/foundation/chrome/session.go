package chromium

import (
	"context"
	"errors"
	"fmt"
	"os/exec"
	"path/filepath"
	"slices"
	"syscall"
	"time"

	"github.com/chromedp/cdproto/runtime"
	"github.com/chromedp/chromedp"
	"github.com/chromedp/chromedp/remote"
)

func browserCommandOptions(prefix []string, command **exec.Cmd) []chromedp.ExecAllocatorOption {
	options := []chromedp.ExecAllocatorOption{
		chromedp.ExecPath(prefix[0]),
		// Do not let the allocator implicitly disable sandboxing when run as root
		chromedp.Flag("no-sandbox", false),
		chromedp.ModifyCmdFunc(func(cmd *exec.Cmd) {
			if len(prefix) > 1 {
				cmd.Args = slices.Concat(prefix, cmd.Args[1:])
			}
			*command = cmd
			cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
			cmd.Cancel = func() error { return syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL) }
			cmd.WaitDelay = 2 * time.Second
		}),
	}
	if len(prefix) > 1 {
		// Flatpak does not forward chromedp's inherited pipe descriptors
		options = append(options, remote.WebSocket,
			chromedp.Flag("remote-debugging-address", "127.0.0.1"))
	}
	return options
}

func killBrowserChildren(command *exec.Cmd) {
	// A launcher can exit while children still own staged databases
	if command != nil && command.Process != nil {
		_ = syscall.Kill(-command.Process.Pid, syscall.SIGKILL)
	}
}

func closeTemporaryBrowser(ctx, browserCtx context.Context, stopAllocator context.CancelFunc, command *exec.Cmd) error {
	if err := chromedp.Cancel(browserCtx); err != nil {
		return fmt.Errorf("close temporary browser: %w", err)
	}
	stopAllocator()
	if err := ctx.Err(); err != nil {
		return err
	}
	if command == nil || command.ProcessState == nil || !command.ProcessState.Success() {
		return errors.New("temporary browser did not exit cleanly")
	}
	return nil
}

func evaluateExtensionCommand(ctx context.Context, prefix []string, directory, profile, pageURL, id, script string, input map[string]any) error {
	if len(prefix) == 0 {
		return errors.New("browser command is required")
	}
	directoryProfile := filepath.Join(profile, extensionStageProfile)
	indexedDB, err := extensionIndexedDBBackend(directoryProfile, id)
	if err != nil {
		return err
	}
	if indexedDB == indexedDBLevelDB|indexedDBSQLite {
		return errors.New("extension has both SQLite and LevelDB IndexedDB stores; refusing to discard either")
	}
	var command *exec.Cmd
	allocatorOptions := append(browserCommandOptions(prefix, &command),
		chromedp.UserDataDir(profile),
		chromedp.Flag("headless", "new"),
		chromedp.Flag("profile-directory", extensionStageProfile),
		chromedp.Flag("no-first-run", true),
		chromedp.Flag("no-default-browser-check", true),
		chromedp.Flag("disable-background-networking", true),
		chromedp.Flag("disable-component-update", true),
		chromedp.Flag("disable-sync", true),
	)
	// Chromium removes the other backend when it opens an IndexedDB bucket
	// https://chromium.googlesource.com/chromium/src/+/main/content/browser/indexed_db/instance/bucket_context.cc
	switch indexedDB {
	case indexedDBSQLite:
		allocatorOptions = append(allocatorOptions, chromedp.Flag("enable-features", "IdbSqliteBackingStore"))
	case indexedDBLevelDB:
		allocatorOptions = append(allocatorOptions, chromedp.Flag("disable-features", "IdbSqliteBackingStore,IdbSqliteOnDiskRollout"))
	}
	if directory != "" {
		allocatorOptions = append(allocatorOptions, chromedp.Flag("load-extension", directory), chromedp.Flag("disable-extensions-except", directory))
	}
	allocatorCtx, stopAllocator := chromedp.NewExecAllocator(ctx, allocatorOptions...)
	browserCtx, stopBrowser := chromedp.NewContext(allocatorCtx)
	defer func() {
		stopBrowser()
		stopAllocator()
		killBrowserChildren(command)
	}()

	if err := chromedp.Do(browserCtx,
		chromedp.Navigate(pageURL),
		chromedp.PollFunction[chromedp.Void](
			`(url, id) => location.href === new URL(url).href && globalThis.chrome?.runtime?.id === id`,
			chromedp.WithPollingArgs(pageURL, id),
			chromedp.WithPollingTimeout(0),
		),
	); err != nil {
		return fmt.Errorf("open extension page: %w", err)
	}
	global, err := chromedp.Run(browserCtx, chromedp.Evaluate[*runtime.RemoteObject]("globalThis"))
	if err != nil {
		return fmt.Errorf("find extension page context: %w", err)
	}
	await := true
	if _, err := chromedp.Run(browserCtx, chromedp.CallFunctionOn[chromedp.Void](
		"async function(input) {\n"+script+"\n}",
		func(params *runtime.CallFunctionOnParams) {
			params.ObjectID = global.ObjectID
			params.AwaitPromise = &await
		},
		input,
	)); err != nil {
		return fmt.Errorf("extension script failed: %w", err)
	}
	// A graceful close flushes databases; cancellation alone kills the browser
	if err := closeTemporaryBrowser(ctx, browserCtx, stopAllocator, command); err != nil {
		return err
	}
	if indexedDB != 0 {
		after, err := extensionIndexedDBBackend(directoryProfile, id)
		if err != nil {
			return err
		}
		if after&^indexedDB != 0 {
			return errors.New("temporary browser changed the extension IndexedDB backend")
		}
	}
	return nil
}
