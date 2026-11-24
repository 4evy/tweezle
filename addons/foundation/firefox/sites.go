package firefox

import (
	"bufio"
	"context"
	_ "embed"
	"encoding/json/jsontext"
	json "encoding/json/v2"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/4evy/tweezle/addons/foundation/sites"
)

const (
	marionetteProtocol = 3 // remote/marionette/server.sys.mjs
	// NewSession waits for Firefox's deferred idle-startup tasks, not just its
	// socket. Match Chromium's maintenance-browser lifecycle budget
	startupTimeout     = 180 * time.Second
	shutdownTimeout    = 15 * time.Second
	defaultSiteTimeout = 30 * time.Second
	// Leave time for Gecko's script-timeout response before closing the socket
	scriptResponseGrace = 5 * time.Second
	startupPollInterval = 50 * time.Millisecond
	// Bound allocations when decoding an unexpected or malformed peer packet
	maxMarionettePacket   = 64 << 20
	maxPacketLengthDigits = 9
)

// ApplySites runs Firefox against an isolated profile. The caller owns staging,
// locking the original profile, and committing only after this returns success
func ApplySites(ctx context.Context, command []string, directory string, entries []sites.Site) error {
	if len(entries) == 0 {
		return nil
	}
	if len(command) == 0 || command[0] == "" {
		return errors.New("browser command is required for Firefox site data")
	}
	if err := sites.Validate(entries); err != nil {
		return err
	}
	inputs := make([]struct {
		body string
		data map[string]any
	}, len(entries))
	for i, entry := range entries {
		body, data, err := entry.Load()
		if err != nil {
			return fmt.Errorf("load Firefox site %s: %w", entry.Origin, err)
		}
		inputs[i].body, inputs[i].data = body, data
	}
	stagedProfile, err := os.Stat(directory)
	if err != nil {
		return err
	}
	// This file exists only in the disposable profile, never the real profile
	const preferences = `user_pref("marionette.port", 0);
user_pref("browser.startup.page", 0);
user_pref("browser.sessionstore.resume_from_crash", false);
user_pref("browser.shell.checkDefaultBrowser", false);
user_pref("browser.newtabpage.enabled", false);
user_pref("startup.homepage_welcome_url", "about:blank");
user_pref("startup.homepage_welcome_url.additional", "");
user_pref("datareporting.policy.dataSubmissionEnabled", false);
user_pref("network.captive-portal-service.enabled", false);
user_pref("network.connectivity-service.enabled", false);
`
	if err := os.WriteFile(filepath.Join(directory, "user.js"), []byte(preferences), 0o600); err != nil {
		return err
	}
	// Each browser operation is independently bounded without summing durations
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	args := make([]string, 0, len(command)+8)
	args = append(args, command[1:]...)
	// Gecko now ignores -no-remote, but older forks still honor it
	args = append(args, "-headless", "-no-remote", "-new-instance", "-offline",
		"-profile", directory, "-marionette", "--remote-allow-system-access", "about:blank")
	process := exec.CommandContext(ctx, command[0], args...)
	process.Stdout = io.Discard
	process.Stderr = io.Discard
	process.Env = slices.DeleteFunc(process.Environ(), inheritedGeckoRestartVariable)
	if err := process.Start(); err != nil {
		return fmt.Errorf("start Firefox site data runner: %w", err)
	}
	done := make(chan struct{})
	var processErr error
	go func() {
		processErr = process.Wait()
		close(done)
	}()
	var client *marionetteClient
	profileVerified := false
	defer func() {
		select {
		case <-done:
		default:
			// Failed scripts are discarded, but still give Firefox a chance to
			// release its profile and stop child processes before forcing exit
			cleanup, stopCleanup := context.WithTimeout(context.Background(), shutdownTimeout)
			defer stopCleanup()
			if profileVerified {
				_, _ = client.call(cleanup, "Marionette:Quit", map[string]any{"flags": []string{"eAttemptQuit"}})
			}
			select {
			case <-done:
			case <-cleanup.Done():
				_ = process.Process.Kill()
				<-done
			}
		}
		if client != nil {
			_ = client.connection.Close()
		}
	}()

	startup, stopStartup := context.WithTimeout(ctx, startupTimeout)
	// Server startup and native session readiness have independent budgets
	client, err = connectMarionette(startup, directory, done)
	stopStartup()
	if err != nil {
		return fmt.Errorf("connect Firefox site data runner: %w", err)
	}
	session, stopSession := context.WithTimeout(ctx, startupTimeout)
	defer stopSession()
	response, err := client.call(session, "WebDriver:NewSession", map[string]any{
		"capabilities": map[string]any{"alwaysMatch": map[string]any{
			"acceptInsecureCerts": false,
		}},
	})
	if err != nil {
		return err
	}
	if err := verifyMarionetteProfile(response, directory, stagedProfile); err != nil {
		return err
	}
	profileVerified = true
	if _, err := client.call(session, "Marionette:SetContext", map[string]any{"value": "chrome"}); err != nil {
		return err
	}
	stopSession()
	for i, entry := range entries {
		// Gecko distinguishes domain cookies by a leading dot. IP literals
		// cannot have subdomains and remain host-only even with explicit Domain
		cookieHosts := make([]string, len(entry.Cookies))
		for i, cookie := range entry.Cookies {
			domain := strings.TrimPrefix(cookie.Domain, ".")
			if domain != "" && net.ParseIP(domain) == nil {
				domain = "." + domain
			}
			cookieHosts[i] = domain
		}
		// Add deadlines as times, avoiding duration overflow for large limits
		responseDeadline := time.Now().Add(siteTimeout(entry)).Add(scriptResponseGrace)
		siteCtx, stopSite := context.WithDeadline(ctx, responseDeadline)
		_, err = client.call(siteCtx, "WebDriver:SetTimeouts", map[string]any{
			"script": siteTimeout(entry).Milliseconds(),
		})
		if err == nil {
			var response jsontext.Value
			response, err = client.call(siteCtx, "WebDriver:ExecuteAsyncScript", map[string]any{
				"script": firefoxSiteExecutionScript, "args": []any{entry.Origin, inputs[i].body, inputs[i].data, cookieHosts},
				"sandbox": "tweezle-sites", "newSandbox": true,
			})
			if err == nil {
				var result struct {
					Value struct {
						OK    bool   `json:"ok"`
						Error string `json:"error"`
					} `json:"value"`
				}
				if err = json.Unmarshal(response, &result); err == nil && !result.Value.OK {
					err = fmt.Errorf("native site script failed: %s", result.Value.Error)
				}
			}
		}
		stopSite()
		if err != nil {
			return fmt.Errorf("configure Firefox site %s: %w", entry.Origin, err)
		}
	}
	shutdown, stopShutdown := context.WithTimeout(ctx, shutdownTimeout)
	defer stopShutdown()
	if _, err := client.call(shutdown, "Marionette:Quit", map[string]any{"flags": []string{"eAttemptQuit"}}); err != nil {
		return fmt.Errorf("quit Firefox site data runner: %w", err)
	}
	select {
	case <-done:
		if processErr != nil {
			return fmt.Errorf("wait for Firefox site data runner: %w", processErr)
		}
		return ctx.Err()
	case <-shutdown.Done():
		return fmt.Errorf("wait for Firefox storage flush: %w", shutdown.Err())
	}
}

// Gecko's startup selects restart and reset profiles before reading -profile
// See toolkit/{profile/nsToolkitProfileService.cpp,xre/nsAppRunner.cpp}
func inheritedGeckoRestartVariable(entry string) bool {
	name, _, _ := strings.Cut(entry, "=")
	switch name {
	case "XRE_PROFILE_PATH", "XRE_PROFILE_LOCAL_PATH", "XRE_RESTART_TO_PROFILE_MANAGER",
		"XRE_RESTARTED_BY_PROFILE_MANAGER", "XRE_RESTARTED_BY_PROFILE_SELECTOR",
		"SELECTABLE_PROFILE_RESET_PATH", "SELECTABLE_PROFILE_RESET_STORE_ID",
		"MOZ_RESET_PROFILE_RESTART", "MOZ_RESET_PROFILE_SESSION", "MOZ_LAUNCHED_CHILD",
		"XUL_APP_FILE", "XRE_BINARY_PATH", "XRE_START_OFFLINE",
		"MOZ_MARIONETTE", "MOZ_MARIONETTE_PREF_STATE_ACROSS_RESTARTS":
		return true
	default:
		return false
	}
}

func verifyMarionetteProfile(response jsontext.Value, directory string, stagedProfile os.FileInfo) error {
	// Marionette reports Gecko's actual ProfD in the NewSession capabilities
	var session struct {
		Capabilities struct {
			Profile string `json:"moz:profile"`
		} `json:"capabilities"`
	}
	if err := json.Unmarshal(response, &session); err != nil {
		return fmt.Errorf("decode Firefox site runner capabilities: %w", err)
	}
	actual := session.Capabilities.Profile
	if filepath.IsAbs(actual) {
		selected, err := os.Stat(actual)
		if err == nil && os.SameFile(stagedProfile, selected) {
			return nil
		}
	}
	return fmt.Errorf("expected staged Firefox profile %q, got %q from Marionette", directory, actual)
}

func siteTimeout(entry sites.Site) time.Duration {
	if entry.TimeoutSeconds > 0 {
		return time.Duration(entry.TimeoutSeconds) * time.Second
	}
	return defaultSiteTimeout
}

type marionetteClient struct {
	connection net.Conn
	reader     *bufio.Reader
	id         int
}

// Stop each cancellation callback before another RPC changes the same socket
// deadline. The returned release function is called exactly once by its owner
func marionetteDeadline(ctx context.Context, connection net.Conn) (func(), error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	deadline, _ := ctx.Deadline()
	if err := connection.SetDeadline(deadline); err != nil {
		return nil, err
	}
	done := make(chan struct{})
	stop := context.AfterFunc(ctx, func() {
		_ = connection.SetDeadline(time.Now())
		close(done)
	})
	return func() {
		if !stop() {
			<-done
		}
	}, nil
}

func connectMarionette(ctx context.Context, directory string, done <-chan struct{}) (*marionetteClient, error) {
	ticker := time.NewTicker(startupPollInterval)
	defer ticker.Stop()
	for {
		port, err := os.ReadFile(filepath.Join(directory, "MarionetteActivePort"))
		if err == nil {
			number, err := strconv.Atoi(strings.TrimSpace(string(port)))
			if err == nil && number > 0 && number <= 65535 {
				connection, err := (&net.Dialer{}).DialContext(ctx, "tcp4", net.JoinHostPort("127.0.0.1", strconv.Itoa(number)))
				if err == nil {
					client := &marionetteClient{connection: connection, reader: bufio.NewReader(connection)}
					release, err := marionetteDeadline(ctx, connection)
					if err != nil {
						_ = connection.Close()
						return nil, err
					}
					greeting, err := client.read()
					release()
					var hello struct {
						Application string `json:"applicationType"`
						Protocol    int    `json:"marionetteProtocol"`
					}
					if err == nil {
						err = json.Unmarshal(greeting, &hello)
					}
					if err == nil && (hello.Application != "gecko" || hello.Protocol != marionetteProtocol) {
						err = errors.New("unsupported Marionette protocol")
					}
					if err != nil {
						_ = connection.Close()
						return nil, err
					}
					return client, nil
				}
			}
		} else if !errors.Is(err, os.ErrNotExist) {
			return nil, err
		}
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-done:
			return nil, errors.New("browser exited before Marionette became available")
		case <-ticker.C:
		}
	}
}

func (client *marionetteClient) read() (jsontext.Value, error) {
	var length strings.Builder
	for {
		character, err := client.reader.ReadByte()
		if err != nil {
			return nil, err
		}
		if character == ':' {
			break
		}
		if character < '0' || character > '9' || length.Len() >= maxPacketLengthDigits {
			return nil, errors.New("invalid Marionette packet length")
		}
		length.WriteByte(character)
	}
	number, err := strconv.Atoi(length.String())
	if err != nil || number <= 0 || number > maxMarionettePacket {
		return nil, errors.New("invalid Marionette packet size")
	}
	data := make(jsontext.Value, number)
	_, err = io.ReadFull(client.reader, data)
	return data, err
}

func (client *marionetteClient) call(ctx context.Context, method string, parameters any) (jsontext.Value, error) {
	release, err := marionetteDeadline(ctx, client.connection)
	if err != nil {
		return nil, fmt.Errorf("set deadline for Marionette %s: %w", method, err)
	}
	defer release()
	client.id++
	packet, err := json.Marshal([]any{0, client.id, method, parameters})
	if err != nil {
		return nil, err
	}
	writer := bufio.NewWriter(client.connection)
	if _, err := fmt.Fprintf(writer, "%d:", len(packet)); err != nil {
		return nil, fmt.Errorf("write packet size for Marionette %s: %w", method, err)
	}
	if _, err := writer.Write(packet); err != nil {
		return nil, fmt.Errorf("write packet for Marionette %s: %w", method, err)
	}
	if err := writer.Flush(); err != nil {
		return nil, fmt.Errorf("flush packet for Marionette %s: %w", method, err)
	}
	data, err := client.read()
	if err != nil {
		return nil, fmt.Errorf("read reply for Marionette %s: %w", method, err)
	}
	var response []jsontext.Value
	if err := json.Unmarshal(data, &response); err != nil {
		return nil, err
	}
	if len(response) != 4 || string(response[0]) != "1" || string(response[1]) != strconv.Itoa(client.id) {
		return nil, errors.New("unexpected Marionette response")
	}
	if string(response[2]) != "null" {
		var failure struct {
			Error   string `json:"error"`
			Message string `json:"message"`
		}
		if err := json.Unmarshal(response[2], &failure); err != nil {
			return nil, err
		}
		return nil, fmt.Errorf("call Marionette %s: %s: %s", method, failure.Error, failure.Message)
	}
	return response[3], nil
}

// Native APIs and transport are implemented upstream in:
// remote/marionette/{server,driver,message}.sys.mjs
// dom/interfaces/storage/nsIDOMStorageManager.idl
// js/xpconnect/src/Sandbox.cpp and netwerk/cookie/nsICookieManager.idl
//
//go:generate bun run --cwd runtime build
//go:embed runtime/generated/script.js
var firefoxSiteScript string

// Marionette supplies arguments and its asynchronous completion callback
var firefoxSiteExecutionScript = firefoxSiteScript +
	"\napplyFirefoxSite(arguments[0], arguments[1], arguments[2], arguments[3], arguments[arguments.length - 1]);"
