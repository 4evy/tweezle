package chromium

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"time"

	"github.com/chromedp/cdproto/browser"
	"github.com/chromedp/cdproto/cdp"
	"github.com/chromedp/chromedp"
	"golang.org/x/net/http/httpguts"
)

const browserProbeTimeout = 15 * time.Second

type browserIdentity struct {
	UserAgent string
	Product   string
}

// readBrowserIdentity asks the installed executable for its native user agent
// without touching the user's profile or synthesizing reduced/frozen tokens
// https://chromedevtools.github.io/devtools-protocol/tot/Browser/#method-getVersion
func readBrowserIdentity(ctx context.Context, launcher string) (browserIdentity, error) {
	return readBrowserIdentityCommand(ctx, func(profile string) ([]string, error) {
		return []string{launcher}, nil
	})
}

func readBrowserIdentityCommand(ctx context.Context, prefix func(string) ([]string, error)) (browserIdentity, error) {
	ctx, cancel := context.WithTimeout(ctx, browserProbeTimeout)
	defer cancel()
	profile, err := os.MkdirTemp("", "tweezle-user-agent-")
	if err != nil {
		return browserIdentity{}, err
	}
	defer removeBrowserProbeProfile(profile)
	commandPrefix, err := prefix(profile)
	if err != nil {
		return browserIdentity{}, err
	}
	if len(commandPrefix) == 0 {
		return browserIdentity{}, fmt.Errorf("browser command is required")
	}
	var command *exec.Cmd
	// Headless mode changes the user agent. Chromium honors no-startup-window
	// even with the allocator's about:blank positional argument
	options := append(browserCommandOptions(commandPrefix, &command),
		chromedp.UserDataDir(profile),
		chromedp.Flag("no-first-run", true),
		chromedp.Flag("no-default-browser-check", true),
		chromedp.Flag("no-startup-window", true),
		chromedp.Flag("disable-background-networking", true),
		chromedp.Flag("disable-component-update", true),
	)
	allocatorCtx, stopAllocator := chromedp.NewExecAllocator(ctx, options...)
	browserCtx, stopBrowser := chromedp.NewContext(allocatorCtx)
	defer func() {
		stopBrowser()
		stopAllocator()
		killBrowserChildren(command)
	}()
	version, err := chromedp.CallBrowser(browserCtx, browser.GetVersion, cdp.Empty{})
	if err != nil {
		return browserIdentity{}, err
	}
	if version.UserAgent == "" || !httpguts.ValidHeaderFieldValue(version.UserAgent) {
		return browserIdentity{}, fmt.Errorf("DevTools returned an invalid user agent")
	}
	return browserIdentity{UserAgent: version.UserAgent, Product: version.Product}, nil
}

func removeBrowserProbeProfile(profile string) {
	// Allow killed children to release the profile before retrying removal
	for range 10 {
		if err := os.RemoveAll(profile); err == nil {
			return
		}
		time.Sleep(25 * time.Millisecond)
	}
}
