package chromium

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync/atomic"
	"time"

	"github.com/4evy/tweezle/addons/foundation/sites"
	"github.com/chromedp/cdproto/cdp"
	"github.com/chromedp/cdproto/fetch"
	"github.com/chromedp/cdproto/network"
	"github.com/chromedp/cdproto/page"
	"github.com/chromedp/cdproto/runtime"
	"github.com/chromedp/cdproto/storage"
	"github.com/chromedp/chromedp"
)

type siteCookieParams struct {
	network.SetCookieParams
	// Do not omit Unix epoch zero, which means expired rather than session
	Expires cdp.TimeSinceEpoch `json:"expires"`
}

type siteCookieResult struct {
	Success *bool `json:"success"`
}

// PreparedSite is the validated configuration and loaded inputs for one site
type PreparedSite struct {
	config sites.Site
	script string
	input  map[string]any
}

// PrepareSites loads every site's inputs before any profile is changed
func PrepareSites(configs []sites.Site) ([]PreparedSite, error) {
	if err := sites.Validate(configs); err != nil {
		return nil, err
	}
	entries := make([]PreparedSite, len(configs))
	for i, config := range configs {
		script, input, err := config.Load()
		if err != nil {
			return nil, fmt.Errorf("load inputs for site %s: %w", config.Origin, err)
		}
		entries[i] = PreparedSite{config: config, script: script, input: input}
	}
	return entries, nil
}

type localStorageBackend uint8

const (
	localStorageLevelDB localStorageBackend = 1 << iota
	localStorageSQLite
)

func stagedLocalStorageBackend(profile string) (localStorageBackend, error) {
	// Chromium opens existing SQLite databases even in the default rollout
	// stage that creates new databases in LevelDB
	// https://chromium.googlesource.com/chromium/src/+/main/components/services/storage/dom_storage/dom_storage_database.cc
	var backend localStorageBackend
	info, err := os.Lstat(filepath.Join(profile, "LocalStorage"))
	if err == nil {
		if !info.Mode().IsRegular() {
			return 0, errors.New("SQLite LocalStorage must be a regular file")
		}
		backend |= localStorageSQLite
	} else if !errors.Is(err, os.ErrNotExist) {
		return 0, err
	}
	info, err = os.Lstat(filepath.Join(profile, "Local Storage", "leveldb"))
	if errors.Is(err, os.ErrNotExist) {
		return backend, nil
	}
	if err != nil {
		return 0, err
	}
	if !info.IsDir() {
		return 0, errors.New("LevelDB LocalStorage must be a directory")
	}
	return backend | localStorageLevelDB, nil
}

// EvaluateSitesCommand mutates only a staged profile, using Chromium's native
// cookie manager and first-party renderer storage. The caller commits the stage
// only after this function has observed a successful, graceful browser exit
func EvaluateSitesCommand(ctx context.Context, prefix []string, root string, entries []PreparedSite) error {
	if len(prefix) == 0 {
		return errors.New("browser command is required")
	}
	profile := filepath.Join(root, siteStageProfile)
	backend, err := stagedLocalStorageBackend(profile)
	if err != nil {
		return err
	}
	var command *exec.Cmd
	proxy, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		return fmt.Errorf("reserve offline proxy: %w", err)
	}
	defer func() { _ = proxy.Close() }()
	go func() {
		for {
			connection, err := proxy.Accept()
			if err != nil {
				return
			}
			_ = connection.Close()
		}
	}()
	allocatorOptions := append(browserCommandOptions(prefix, &command),
		chromedp.UserDataDir(root),
		chromedp.Flag("headless", "new"),
		chromedp.Flag("profile-directory", siteStageProfile),
		chromedp.Flag("no-first-run", true),
		chromedp.Flag("no-default-browser-check", true),
		chromedp.Flag("disable-background-networking", true),
		chromedp.Flag("disable-component-update", true),
		chromedp.Flag("disable-sync", true),
		chromedp.Flag("disable-extensions", true),
		// Preserve existing session cookies in the staged database, without
		// copying session tabs or changing the original profile's preferences
		// chromium/chromium@583bbf02cefc4bb6abd92f546d4d872f83725193:
		// chrome/browser/profiles/profile_impl.cc, ShouldRestoreOldSessionCookies
		chromedp.Flag("restore-last-session", true),
		// Fetch intercepts the synthetic document before network dispatch. The
		// rejecting loopback proxy and resolver also block other HTTP traffic
		chromedp.Flag("proxy-server", "http://"+proxy.Addr().String()),
		chromedp.Flag("proxy-bypass-list", "<-loopback>"),
		chromedp.Flag("host-resolver-rules", "MAP * ~NOTFOUND"),
		chromedp.Flag("disable-quic", true),
	)
	switch {
	case backend&localStorageSQLite != 0:
		allocatorOptions = append(allocatorOptions, chromedp.Flag("enable-features", "DomStorageSqlite"))
	case backend&localStorageLevelDB != 0:
		allocatorOptions = append(allocatorOptions, chromedp.Flag("disable-features",
			"DomStorageSqlite,DomStorageSqliteMigration,DomStorageSqliteNewDatabases"))
	}
	allocatorCtx, stopAllocator := chromedp.NewExecAllocator(ctx, allocatorOptions...)
	browserCtx, stopBrowser := chromedp.NewContext(allocatorCtx)
	defer func() {
		stopBrowser()
		stopAllocator()
		killBrowserChildren(command)
	}()

	if _, err := chromedp.Call(browserCtx, network.Enable, network.EnableParams{}); err != nil {
		return fmt.Errorf("start temporary browser: %w", err)
	}
	if _, err := chromedp.Call(browserCtx, network.SetBypassServiceWorker, network.SetBypassServiceWorkerParams{Bypass: true}); err != nil {
		return fmt.Errorf("bypass service workers: %w", err)
	}
	if _, err := chromedp.Call(browserCtx, network.SetCacheDisabled, network.SetCacheDisabledParams{CacheDisabled: true}); err != nil {
		return err
	}
	tree, err := chromedp.Call(browserCtx, page.GetFrameTree, cdp.Empty{})
	if err != nil {
		return err
	}
	mainFrame := tree.FrameTree.Frame.ID
	var documentURL atomic.Value
	documentURL.Store("")
	eventCtx, stopEvents := context.WithCancel(browserCtx)
	defer stopEvents()
	events := chromedp.Events(eventCtx, fetch.RequestPaused)
	interceptionDone := make(chan error, 1)
	go func() {
		for event, err := range events {
			if err != nil {
				interceptionDone <- err
				return
			}
			// chromium/chromium@583bbf02cefc4bb6abd92f546d4d872f83725193:
			// content/browser/devtools/protocol/fetch_handler.cc,
			// FetchHandler::FulfillRequest supplies an intercepted response
			if event.ResourceType == network.ResourceTypeDocument && event.FrameID == mainFrame && event.Request.URL == documentURL.Load().(string) {
				_, err = chromedp.Call(eventCtx, fetch.FulfillRequest, fetch.FulfillRequestParams{
					RequestID:    event.RequestID,
					ResponseCode: 200,
					ResponseHeaders: []*fetch.HeaderEntry{
						{Name: "Content-Type", Value: "text/html; charset=utf-8"},
						{Name: "Cache-Control", Value: "no-store"},
						{Name: "Content-Security-Policy", Value: "default-src 'none'; script-src 'unsafe-eval'; connect-src 'none'; worker-src 'none'; frame-src 'none'"},
					},
					Body: []byte("<!doctype html><title>Tweezle site data</title>"),
				})
			} else {
				_, err = chromedp.Call(eventCtx, fetch.FailRequest, fetch.FailRequestParams{RequestID: event.RequestID, ErrorReason: network.ErrorReasonBlockedByClient})
			}
			if err != nil {
				interceptionDone <- err
				return
			}
		}
		interceptionDone <- nil
	}()
	if _, err := chromedp.Call(browserCtx, fetch.Enable, fetch.EnableParams{
		Patterns: []*fetch.RequestPattern{{URLPattern: "*", RequestStage: fetch.RequestStageRequest}},
	}); err != nil {
		return fmt.Errorf("intercept site traffic: %w", err)
	}
	for _, entry := range entries {
		config := entry.config
		documentURL.Store(config.Origin + "/")
		const defaultSiteTimeout = 180 * time.Second
		timeout := defaultSiteTimeout
		if config.TimeoutSeconds > 0 {
			timeout = time.Duration(config.TimeoutSeconds) * time.Second
		}
		operationCtx, cancel := context.WithTimeout(browserCtx, timeout)
		err := evaluateSite(operationCtx, entry)
		cancel()
		if err != nil {
			return fmt.Errorf("configure site %s: %w", config.Origin, err)
		}
		select {
		case err := <-interceptionDone:
			return fmt.Errorf("site request interception stopped: %w", err)
		default:
		}
	}
	stopEvents()
	if err := <-interceptionDone; err != nil && !errors.Is(err, context.Canceled) {
		return fmt.Errorf("intercept site traffic: %w", err)
	}
	// Closing, rather than canceling the allocator, flushes native databases
	if err := closeTemporaryBrowser(ctx, browserCtx, stopAllocator, command); err != nil {
		return err
	}
	// An older executable may ignore a feature flag. Never commit a newly
	// created alternate database over an existing backend
	if backend != 0 {
		after, err := stagedLocalStorageBackend(profile)
		if err != nil {
			return err
		}
		if after&^backend != 0 {
			return errors.New("temporary browser changed the LocalStorage backend")
		}
	}
	return nil
}

const siteStageProfile = "profile"

func evaluateSite(ctx context.Context, entry PreparedSite) error {
	config := entry.config
	if err := chromedp.Do(ctx, chromedp.Navigate(config.Origin+"/")); err != nil {
		return fmt.Errorf("open synthetic site document: %w", err)
	}
	origin, err := chromedp.Run(ctx, chromedp.Evaluate[string]("location.origin"))
	if err != nil {
		return err
	}
	if origin != config.Origin {
		return fmt.Errorf("synthetic document has origin %q, want %q", origin, config.Origin)
	}
	if err := applySiteCookies(ctx, config); err != nil {
		return err
	}
	global, err := chromedp.Run(ctx, chromedp.Evaluate[*runtime.RemoteObject]("globalThis"))
	if err != nil {
		return err
	}
	await := true
	_, err = chromedp.Run(ctx, chromedp.CallFunctionOn[chromedp.Void](
		"async function(input) {\n"+entry.script+"\n}",
		func(params *runtime.CallFunctionOnParams) {
			params.ObjectID = global.ObjectID
			params.AwaitPromise = &await
		}, entry.input,
	))
	return err
}

func applySiteCookies(ctx context.Context, config sites.Site) error {
	origin, err := url.Parse(config.Origin)
	if err != nil {
		return err
	}
	if config.ClearCookies {
		cookies, err := chromedp.Call(ctx, storage.GetCookies, storage.GetCookiesParams{})
		if err != nil {
			return err
		}
		for _, cookie := range cookies.Cookies {
			if cookie.PartitionKey != nil || cookie.PartitionKeyOpaque || !siteCookieDomainMatches(origin.Hostname(), cookie.Domain) {
				continue
			}
			if err := deleteSiteCookie(ctx, cookie.Name, cookie.Domain, cookie.Path); err != nil {
				return err
			}
		}
	}
	for _, cookie := range config.Cookies {
		domain := siteCookieDomain(origin.Hostname(), cookie.Domain)
		path := cookie.Path
		if path == "" {
			path = "/"
		}
		if cookie.Remove {
			if err := deleteSiteCookie(ctx, cookie.Name, domain, path); err != nil {
				return err
			}
			continue
		}
		secure := origin.Scheme == "https"
		if cookie.Secure != nil {
			secure = *cookie.Secure
		}
		sameSite := network.CookieSameSiteLax
		switch cookie.SameSite {
		case "strict":
			sameSite = network.CookieSameSiteStrict
		case "none":
			sameSite = network.CookieSameSiteNone
		}
		params := siteCookieParams{
			Name: cookie.Name, Value: cookie.Value, Domain: domain,
			Path: path, Secure: &secure, HTTPOnly: &cookie.HTTPOnly,
			SameSite: sameSite,
			Expires:  cdp.TimeSinceEpoch(*cookie.Expires),
		}
		// Domain alone preserves explicit secure=false on HTTPS origins
		// MakeCookieFromProtocolValues otherwise forces secure for HTTPS URLs
		// Older Chromium versions return success=false instead of an error
		result, err := chromedp.Call(ctx, cdp.Command[siteCookieParams, siteCookieResult]{
			Method: network.CommandSetCookie,
		}, params)
		if err != nil {
			return fmt.Errorf("set cookie %q: %w", cookie.Name, err)
		}
		if result.Success != nil && !*result.Success {
			return fmt.Errorf("native cookie manager rejected cookie %q", cookie.Name)
		}
	}
	return nil
}

func siteCookieDomain(host, configured string) string {
	if configured != "" {
		configured = strings.TrimPrefix(configured, ".")
		if net.ParseIP(configured) == nil {
			return "." + configured
		}
		return configured
	}
	if strings.Contains(host, ":") {
		return "[" + host + "]"
	}
	return host
}

func siteCookieDomainMatches(host, domain string) bool {
	domain = strings.TrimPrefix(strings.TrimSuffix(domain, "]"), "[")
	if domain, ok := strings.CutPrefix(domain, "."); ok {
		return host == domain || strings.HasSuffix(host, "."+domain)
	}
	return host == domain
}

func deleteSiteCookie(ctx context.Context, name, domain, path string) error {
	// chromium/chromium@583bbf02cefc4bb6abd92f546d4d872f83725193:
	// content/browser/devtools/protocol/network_handler.cc, DeleteCookies and
	// FilterCookies: absent partitionKey selects only unpartitioned cookies
	_, err := chromedp.Call(ctx, network.DeleteCookies, network.DeleteCookiesParams{
		Name: name, Domain: domain, Path: path,
	})
	if err != nil {
		return fmt.Errorf("delete cookie %q: %w", name, err)
	}
	return nil
}
