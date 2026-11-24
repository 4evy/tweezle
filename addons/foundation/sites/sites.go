// Package sites describes first-party website data applied through native
// browser APIs, rather than editing browser-owned database formats
package sites

import (
	"bytes"
	_ "embed"
	"encoding/json/jsontext"
	"errors"
	"fmt"
	"io"
	"math"
	"net/netip"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	jsonutil "github.com/4evy/tweezle/internal/json"
	"golang.org/x/net/http/httpguts"
	"golang.org/x/net/idna"
)

// JavaScript numbers represent integers exactly only through 2^53 - 1
const maxJavaScriptInteger = 1<<53 - 1

// Gecko consumes cookie expiry in milliseconds, so conversion must remain exact
const millisecondsPerSecond = 1000
const maxCookieExpirySeconds = maxJavaScriptInteger / millisecondsPerSecond

// Site targets the unpartitioned, default-container storage of one exact origin
// Script is an optional async JavaScript body run after declarative mutations
// Scripts and JSON input files resolve relative to the declaring configuration
// Session storage is intentionally absent because it cannot persist offline
// https://github.com/chromium/chromium/blob/main/content/browser/devtools/protocol/storage_handler.cc
// https://github.com/mozilla-firefox/firefox/blob/main/remote/webdriver-bidi/modules/root/storage.sys.mjs
type Site struct {
	Origin             string            `toml:"origin" json:"origin"`
	Cookies            []Cookie          `toml:"cookies" json:"cookies,omitempty"`
	ClearCookies       bool              `toml:"clear_cookies" json:"clear_cookies,omitempty"`
	LocalStorage       map[string]string `toml:"local_storage" json:"local_storage,omitempty"`
	LocalStorageRemove []string          `toml:"local_storage_remove" json:"local_storage_remove,omitempty"`
	ClearLocalStorage  bool              `toml:"clear_local_storage" json:"clear_local_storage,omitempty"`
	IndexedDB          []Database        `toml:"indexed_db" json:"indexed_db,omitempty"`
	Script             string            `toml:"script" json:"-"`
	Files              map[string]string `toml:"files" json:"-"`
	TimeoutSeconds     int               `toml:"timeout_seconds" json:"-"`
}

type Cookie struct {
	Name     string `toml:"name" json:"name"`
	Value    string `toml:"value" json:"value"`
	Domain   string `toml:"domain" json:"domain,omitempty"`
	Path     string `toml:"path" json:"path,omitempty"`
	Secure   *bool  `toml:"secure" json:"secure,omitempty"`
	HTTPOnly bool   `toml:"http_only" json:"http_only,omitempty"`
	SameSite string `toml:"same_site" json:"same_site,omitempty"`
	Expires  *int64 `toml:"expires" json:"expires,omitempty"`
	Remove   bool   `toml:"remove" json:"remove,omitempty"`
}

type Database struct {
	Name    string  `toml:"name" json:"name"`
	Version int64   `toml:"version" json:"version,omitempty"`
	Delete  bool    `toml:"delete" json:"delete,omitempty"`
	Stores  []Store `toml:"stores" json:"stores,omitempty"`
}

type Store struct {
	Name          string   `toml:"name" json:"name"`
	KeyPath       any      `toml:"key_path" json:"key_path,omitzero"`
	AutoIncrement bool     `toml:"auto_increment" json:"auto_increment,omitempty"`
	Delete        bool     `toml:"delete" json:"delete,omitempty"`
	Clear         bool     `toml:"clear" json:"clear,omitempty"`
	Records       []Record `toml:"records" json:"records,omitempty"`
	Indexes       []Index  `toml:"indexes" json:"indexes,omitempty"`
}

type Record struct {
	Key    any  `toml:"key" json:"key,omitzero"`
	Value  any  `toml:"value" json:"value"`
	Delete bool `toml:"delete" json:"delete,omitempty"`
}

type Index struct {
	Name       string `toml:"name" json:"name"`
	KeyPath    any    `toml:"key_path" json:"key_path"`
	Unique     bool   `toml:"unique" json:"unique,omitempty"`
	MultiEntry bool   `toml:"multi_entry" json:"multi_entry,omitempty"`
}

//go:generate bun run --cwd runtime build
//go:embed runtime/generated/apply.js
var applyScript string

var builtInScript = applyScript + "\nawait applySite(input);\n"
var dnsProfile = idna.New(idna.MapForLookup(), idna.BidiRule(), idna.VerifyDNSLength(true))

// ResolvePaths expands only file paths, leaving website data untouched
func ResolvePaths(configs []Site, base string) {
	resolve := func(path string) string {
		if path == "" || filepath.IsAbs(path) {
			return path
		}
		return filepath.Join(base, path)
	}
	for i := range configs {
		configs[i].Script = resolve(configs[i].Script)
		for key, path := range configs[i].Files {
			configs[i].Files[key] = resolve(path)
		}
	}
}

func (site Site) Load() (string, map[string]any, error) {
	files := make(map[string]any, len(site.Files))
	body, err := site.readInputs(files)
	if err != nil {
		return "", nil, err
	}
	script := builtInScript
	if len(body) > 0 {
		script += "\nawait (async function(input) {\n" + string(body) + "\n})(input);\n"
	}
	return script, map[string]any{"site": site, "files": files}, nil
}

// ValidateFiles checks referenced files without constructing runtime scripts
// or inputs. Call Validate separately to check configuration structure
func ValidateFiles(configs []Site) error {
	var errs []error
	for i, site := range configs {
		if _, err := site.readInputs(nil); err != nil {
			errs = append(errs, fmt.Errorf("sites[%d]: %w", i, err))
		}
	}
	return errors.Join(errs...)
}

func (site Site) readInputs(files map[string]any) ([]byte, error) {
	for name, path := range site.Files {
		file, err := os.Open(path)
		if err != nil {
			return nil, fmt.Errorf("read site input %s: %w", name, err)
		}
		if files == nil {
			err = checkJSONInput(file)
		} else {
			files[name], err = jsonutil.Default.Decode[any](file)
		}
		if err := errors.Join(err, file.Close()); err != nil {
			return nil, fmt.Errorf("decode site input %s: %w", name, err)
		}
	}
	if site.Script == "" {
		return nil, nil
	}
	body, err := os.ReadFile(site.Script)
	if err != nil {
		return nil, fmt.Errorf("read site script: %w", err)
	}
	if len(bytes.TrimSpace(body)) == 0 {
		return nil, errors.New("site script must not be empty")
	}
	return body, nil
}

func checkJSONInput(file io.Reader) error {
	decoder := jsontext.NewDecoder(file)
	if err := decoder.SkipValue(); err != nil {
		return err
	}
	_, err := decoder.ReadToken()
	if err == io.EOF {
		return nil
	}
	if err == nil {
		return errors.New("unexpected data after JSON document")
	}
	return err
}

// Validate checks site structure and rejects duplicate canonical origins
func Validate(configs []Site) error {
	seen := map[string]bool{}
	var errs []error
	for i, site := range configs {
		if err := site.validate(); err != nil {
			errs = append(errs, fmt.Errorf("sites[%d]: %w", i, err))
		}
		if seen[site.Origin] {
			errs = append(errs, fmt.Errorf("duplicate site origin %q", site.Origin))
		}
		seen[site.Origin] = true
	}
	return errors.Join(errs...)
}

func (site Site) validate() error {
	scheme, host, hostIP, err := parseOrigin(site.Origin)
	if err != nil {
		return err
	}
	if site.TimeoutSeconds < 0 || int64(site.TimeoutSeconds) > math.MaxInt64/int64(time.Second) {
		return errors.New("timeout_seconds must be a nonnegative supported duration")
	}
	var errs []error
	// Host-only scope is part of cookie identity, separate from the domain
	type cookieKey struct {
		domain, path, name string
		hostOnly           bool
	}
	cookies := map[cookieKey]bool{}
	for i, cookie := range site.Cookies {
		domain, path := strings.TrimPrefix(cookie.Domain, "."), cookie.Path
		if err := validateCookie(scheme, host, hostIP, cookie, domain); err != nil {
			errs = append(errs, fmt.Errorf("cookies[%d]: %w", i, err))
			continue
		}
		hostOnly := domain == "" || hostIP
		if domain == "" {
			domain = host
		}
		if path == "" {
			path = "/"
		}
		key := cookieKey{domain: domain, path: path, name: cookie.Name, hostOnly: hostOnly}
		if cookies[key] {
			errs = append(errs, fmt.Errorf("duplicate cookie %q", cookie.Name))
		}
		cookies[key] = true
	}
	databases := map[string]bool{}
	for _, database := range site.IndexedDB {
		if database.Name == "" || databases[database.Name] {
			errs = append(errs, fmt.Errorf("empty or duplicate IndexedDB database %q", database.Name))
		}
		databases[database.Name] = true
		if database.Version < 0 || database.Version > maxJavaScriptInteger {
			errs = append(errs, errors.New("IndexedDB version must be a nonnegative JavaScript safe integer"))
		}
		if database.Delete && (database.Version != 0 || len(database.Stores) != 0) {
			errs = append(errs, errors.New("deleted database must not specify version or stores"))
		}
		stores := map[string]bool{}
		for _, store := range database.Stores {
			if store.Name == "" || stores[store.Name] {
				errs = append(errs, fmt.Errorf("empty or duplicate IndexedDB store %q", store.Name))
			}
			stores[store.Name] = true
			if store.KeyPath != nil && !validKeyPath(store.KeyPath) {
				errs = append(errs, fmt.Errorf("store %q key_path must be a string or nonempty string array", store.Name))
			}
			if store.Delete && (len(store.Records) != 0 || len(store.Indexes) != 0 || store.Clear || store.KeyPath != nil || store.AutoIncrement) {
				errs = append(errs, fmt.Errorf("deleted store %q must not specify data or schema", store.Name))
			}
			indexes := map[string]bool{}
			for _, index := range store.Indexes {
				if index.Name == "" || indexes[index.Name] || !validKeyPath(index.KeyPath) {
					errs = append(errs, fmt.Errorf("store %q has an invalid or duplicate index %q", store.Name, index.Name))
				}
				indexes[index.Name] = true
			}
			for _, record := range store.Records {
				if (record.Delete && record.Key == nil) || (record.Key != nil && !validKey(record.Key)) {
					errs = append(errs, fmt.Errorf("store %q record key must be a string, finite number, or array of keys", store.Name))
				}
			}
		}
	}
	for name, path := range site.Files {
		if name == "" || path == "" {
			errs = append(errs, errors.New("site files require nonempty names and paths"))
		}
	}
	return errors.Join(errs...)
}

func parseOrigin(raw string) (scheme, host string, hostIP bool, err error) {
	u, err := url.Parse(raw)
	const message = "origin must be an exact canonical HTTP(S) origin, e.g. https://example.com, without path, credentials, query, or fragment"
	if err != nil || (u.Scheme != "https" && u.Scheme != "http") ||
		raw != u.Scheme+"://"+u.Host || u.Host == "" || strings.HasSuffix(u.Host, ":") {
		return "", "", false, errors.New(message)
	}
	host = u.Hostname()
	ip, ok := canonicalHost(host)
	if !ok || (u.Host[0] == '[') != ip.Is6() {
		return "", "", false, errors.New(message)
	}
	if port := u.Port(); port != "" {
		n, err := strconv.Atoi(port)
		if err != nil || n < 1 || n > 65535 || strconv.Itoa(n) != port || (u.Scheme == "http" && n == 80) || (u.Scheme == "https" && n == 443) {
			return "", "", false, errors.New("origin must use a valid non-default port without leading zeroes")
		}
	}
	return u.Scheme, host, ip.IsValid(), nil
}

func canonicalHost(host string) (netip.Addr, bool) {
	if ip, err := netip.ParseAddr(host); err == nil {
		// Mapped IPv6 uses a different serialization in browsers and netip
		return ip, ip.Zone() == "" && !ip.Is4In6() && host == ip.String()
	}
	if host == "" || strings.HasSuffix(host, ".") {
		return netip.Addr{}, false
	}
	ascii, err := dnsProfile.ToASCII(host)
	if err != nil || ascii != host {
		return netip.Addr{}, false
	}
	// Browsers interpret DNS-shaped hosts ending in a number as IPv4,
	// including legacy shorthand, octal, and hexadecimal addresses
	label := host[strings.LastIndexByte(host, '.')+1:]
	if digits := strings.TrimPrefix(label, "0x"); digits != label {
		return netip.Addr{}, strings.Trim(digits, "0123456789abcdef") != ""
	}
	return netip.Addr{}, strings.Trim(label, "0123456789") != ""
}

func validateCookie(scheme, host string, hostIP bool, cookie Cookie, domain string) error {
	if !httpguts.ValidHeaderFieldName(cookie.Name) || strings.ContainsAny(cookie.Value, "\x00\r\n;") {
		return errors.New("cookie requires a valid name and a value without NUL, CR, LF, or semicolon")
	}
	if cookie.Path != "" && (!strings.HasPrefix(cookie.Path, "/") || strings.ContainsAny(cookie.Path, "\x00\r\n;")) {
		return errors.New("cookie path must start with / and contain no control separators")
	}
	if cookie.Domain != "" {
		ip, ok := canonicalHost(domain)
		if !ok || ip.Is6() || (domain != host && (hostIP || ip.IsValid() || !strings.HasSuffix(host, "."+domain))) {
			return errors.New("cookie domain must match the origin host or a canonical parent domain")
		}
	}
	secure := scheme == "https"
	if cookie.Secure != nil {
		secure = *cookie.Secure
	}
	if cookie.SameSite != "" && cookie.SameSite != "strict" && cookie.SameSite != "lax" && cookie.SameSite != "none" {
		return errors.New("cookie same_site must be strict, lax, or none")
	}
	if cookie.SameSite == "none" && !secure {
		return errors.New("same_site = none requires a secure cookie")
	}
	if secure && scheme != "https" {
		return errors.New("secure cookies require an HTTPS origin")
	}
	if strings.HasPrefix(cookie.Name, "__Secure-") && !secure {
		return errors.New("__Secure- cookies require secure = true")
	}
	if strings.HasPrefix(cookie.Name, "__Host-") && (!secure || cookie.Domain != "" || (cookie.Path != "" && cookie.Path != "/")) {
		return errors.New("__Host- cookies require secure, host-only scope, and path /")
	}
	if cookie.Expires != nil && (*cookie.Expires < 0 || *cookie.Expires > maxCookieExpirySeconds) {
		return errors.New("cookie expires must be nonnegative Unix seconds exactly representable in milliseconds")
	}
	if !cookie.Remove && cookie.Expires == nil {
		return errors.New("persistent site cookies require expires as Unix seconds; session cookies cannot be configured offline")
	}
	return nil
}

func validKeyPath(value any) bool {
	switch value := value.(type) {
	case string:
		return true
	case []any:
		if len(value) == 0 {
			return false
		}
		for _, part := range value {
			if _, ok := part.(string); !ok {
				return false
			}
		}
		return true
	case []string:
		return len(value) > 0
	default:
		return false
	}
}

func validKey(value any) bool {
	switch value := value.(type) {
	case string:
		return true
	case int64:
		return value >= -maxJavaScriptInteger && value <= maxJavaScriptInteger
	case int:
		return int64(value) >= -maxJavaScriptInteger && int64(value) <= maxJavaScriptInteger
	case float64:
		return !math.IsNaN(value) && !math.IsInf(value, 0)
	case []any:
		for _, part := range value {
			if !validKey(part) {
				return false
			}
		}
		return true
	case []string:
		return true
	default:
		return false
	}
}
