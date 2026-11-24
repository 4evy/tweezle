package profile

import (
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"net/netip"
	"strconv"
	"strings"

	"golang.org/x/net/idna"
)

const (
	cookieDefaultPreference    = "profile.default_content_setting_values.cookies"
	cookieExceptionsPreference = "profile.content_settings.exceptions.cookies"
	cookieControlsPreference   = "profile.cookie_controls_mode"
	cookieExceptionSettingKey  = "setting"
	cookiePatternWildcard      = ",*"

	contentSettingAllow       = 1
	contentSettingBlock       = 2
	contentSettingSessionOnly = 4

	thirdPartyCookieModeOff           = 0
	thirdPartyCookieModeBlock         = 1
	thirdPartyCookieModeIncognitoOnly = 2

	cookieSettingChoices          = "allow, block, or session_only"
	thirdPartyCookiePolicyChoices = "off, block, or incognito_only"
)

type CookieSetting string

const (
	CookieSettingAllow       CookieSetting = "allow"
	CookieSettingBlock       CookieSetting = "block"
	CookieSettingSessionOnly CookieSetting = "session_only"
)

type ThirdPartyCookiePolicy string

// Chromium 157 treats off and incognito_only alike in incognito profiles
// Retain both stored enum values for compatibility with other browser builds
const (
	ThirdPartyCookiePolicyOff           ThirdPartyCookiePolicy = "off"
	ThirdPartyCookiePolicyBlock         ThirdPartyCookiePolicy = "block"
	ThirdPartyCookiePolicyIncognitoOnly ThirdPartyCookiePolicy = "incognito_only"
)

var cookieSettingValues = map[CookieSetting]int{
	CookieSettingAllow:       contentSettingAllow,
	CookieSettingBlock:       contentSettingBlock,
	CookieSettingSessionOnly: contentSettingSessionOnly,
}

var thirdPartyCookieModes = map[ThirdPartyCookiePolicy]int{
	ThirdPartyCookiePolicyOff:           thirdPartyCookieModeOff,
	ThirdPartyCookiePolicyBlock:         thirdPartyCookieModeBlock,
	ThirdPartyCookiePolicyIncognitoOnly: thirdPartyCookieModeIncognitoOnly,
}

type CookiePolicy struct {
	Default     CookieSetting          `toml:"default"`
	ThirdParty  ThirdPartyCookiePolicy `toml:"third_party"`
	Allow       []string               `toml:"allow"`
	Block       []string               `toml:"block"`
	SessionOnly []string               `toml:"session_only"`
}

type cookieExceptionRule struct {
	setting  CookieSetting
	patterns []string
}

func (config CookiePolicy) Validate() error {
	var errs []error
	if config.Default != "" && !config.Default.valid() {
		errs = append(errs, fmt.Errorf(
			"browser.preferences.cookies.default must be one of %s, got %q",
			cookieSettingChoices,
			config.Default,
		))
	}
	if !config.ThirdParty.valid() {
		errs = append(errs, fmt.Errorf(
			"browser.preferences.cookies.third_party must be one of %s, got %q",
			thirdPartyCookiePolicyChoices,
			config.ThirdParty,
		))
	}
	seen := map[string]string{}
	for _, rule := range config.exceptionRules() {
		setting, patterns := string(rule.setting), rule.patterns
		for index, pattern := range patterns {
			if strings.TrimSpace(pattern) == "" {
				errs = append(errs, fmt.Errorf("browser.preferences.cookies.%s[%d] must not be empty", setting, index))
				continue
			}
			canonical, err := canonicalCookiePattern(pattern)
			if err != nil {
				errs = append(errs, fmt.Errorf("browser.preferences.cookies.%s[%d]: %w", setting, index, err))
				continue
			}
			if previous, exists := seen[canonical]; exists && previous != setting {
				errs = append(errs, fmt.Errorf(
					"cookie pattern %q appears in both %s and %s",
					pattern,
					previous,
					setting,
				))
			}
			seen[canonical] = setting
		}
	}
	return errors.Join(errs...)
}

func (config CookiePolicy) HasPolicy() bool {
	return config.Default != "" ||
		config.ThirdParty != "" ||
		config.Allow != nil ||
		config.Block != nil ||
		config.SessionOnly != nil
}

func (config CookiePolicy) exceptionRules() []cookieExceptionRule {
	return []cookieExceptionRule{
		{setting: CookieSettingAllow, patterns: config.Allow},
		{setting: CookieSettingBlock, patterns: config.Block},
		{setting: CookieSettingSessionOnly, patterns: config.SessionOnly},
	}
}

func SetCookieAllowlist(preferences map[string]any, patterns []string) error {
	return SetCookiePolicy(preferences, CookiePolicy{Allow: patterns})
}

// SetCookiePolicy updates Chromium's default cookie setting, third-party
// cookie mode, and per-pattern exceptions. A nil exception list is unmanaged.
// A non-nil list owns exceptions with that setting, including when the list is
// empty and therefore removes existing exceptions of that setting.
func SetCookiePolicy(preferences map[string]any, policy CookiePolicy) error {
	if err := policy.Validate(); err != nil {
		return err
	}
	if policy.Default != "" {
		if err := SetNestedValue(
			preferences,
			cookieDefaultPreference,
			policy.Default.contentSettingValueOrZero(),
		); err != nil {
			return fmt.Errorf("set default cookie policy: %w", err)
		}
	}
	if err := setThirdPartyCookiePolicy(preferences, policy.ThirdParty); err != nil {
		return err
	}
	for _, rule := range policy.exceptionRules() {
		if rule.patterns == nil {
			continue
		}
		if err := setCookieExceptions(
			preferences,
			rule.patterns,
			rule.setting.contentSettingValueOrZero(),
		); err != nil {
			return fmt.Errorf("set %s cookie exceptions: %w", rule.setting, err)
		}
	}
	return nil
}

func setThirdPartyCookiePolicy(
	preferences map[string]any,
	policy ThirdPartyCookiePolicy,
) error {
	if policy == "" {
		return nil
	}
	mode, valid := policy.mode()
	if !valid {
		mode = thirdPartyCookieModeOff
	}
	if err := SetNestedValue(preferences, cookieControlsPreference, mode); err != nil {
		return fmt.Errorf("set cookie controls mode: %w", err)
	}
	return nil
}

func setCookieExceptions(preferences map[string]any, patterns []string, setting int) error {
	configured := map[string]struct{}{}
	for _, pattern := range patterns {
		canonical, err := canonicalCookiePattern(pattern)
		if err != nil {
			return err
		}
		configured[canonical] = struct{}{}
	}
	exceptions, err := NestedObject(preferences, cookieExceptionsPreference)
	if err != nil {
		return fmt.Errorf("open cookie exceptions: %w", err)
	}
	// Chromium drops noncanonical twins on load. Remove them here even if
	// they hold a different setting than the rule replacing them
	maps.DeleteFunc(exceptions, func(pattern string, entry any) bool {
		_, managed := configured[pattern]
		if !managed {
			if canonical, err := canonicalCookiePattern(pattern); err == nil {
				if _, replaced := configured[canonical]; replaced {
					return true
				}
			}
		}
		return !managed && isCookieException(entry, setting)
	})
	for pattern := range configured {
		entry, ok := exceptions[pattern].(map[string]any)
		if !ok {
			entry = map[string]any{}
			exceptions[pattern] = entry
		}
		entry[cookieExceptionSettingKey] = setting
	}
	return nil
}

func canonicalCookiePattern(pattern string) (string, error) {
	primary, secondary, pair := strings.Cut(strings.TrimSpace(pattern), ",")
	first, err := canonicalCookieEndpoint(strings.TrimSpace(primary))
	if err != nil {
		return "", fmt.Errorf("invalid cookie pattern %q: %w", pattern, err)
	}
	if !pair {
		return first + cookiePatternWildcard, nil
	}
	second, err := canonicalCookieEndpoint(strings.TrimSpace(secondary))
	if err != nil {
		return "", fmt.Errorf("invalid cookie pattern %q: %w", pattern, err)
	}
	return first + "," + second, nil
}

// Chromium serializes wildcard schemes and ports by omitting them. Accept the
// web subset of ContentSettingsPattern and reject other schemes and URL paths
func canonicalCookieEndpoint(pattern string) (string, error) {
	if pattern == "" {
		return "", errors.New("pattern must not be empty")
	}
	scheme, authority, explicit := strings.Cut(pattern, "://")
	if explicit {
		scheme = strings.ToLower(scheme)
		if scheme != "*" && scheme != "http" && scheme != "https" {
			return "", errors.New("pattern scheme must be http, https, or *")
		}
	} else {
		scheme, authority = "*", pattern
	}
	authority = strings.TrimSuffix(strings.TrimSuffix(authority, "/*"), "/")
	if strings.ContainsAny(authority, "/?#@\\%,") {
		return "", errors.New("patterns cannot contain URL paths, queries, or credentials")
	}
	host, port, hasPort := authority, "", false
	wildcard := strings.HasPrefix(host, "[*.]")
	if wildcard {
		host = strings.TrimPrefix(host, "[*.]")
	}
	if strings.HasPrefix(host, "[") {
		address, rest, ok := strings.Cut(host[1:], "]")
		ip, err := netip.ParseAddr(address)
		if !ok || wildcard || err != nil || !ip.Is6() || ip.Zone() != "" || (rest != "" && !strings.HasPrefix(rest, ":")) {
			return "", errors.New("invalid IPv6 host pattern")
		}
		host = "[" + ip.String() + "]"
		port, hasPort = strings.CutPrefix(rest, ":")
	} else {
		host, port, hasPort = strings.Cut(host, ":")
		switch ip, err := netip.ParseAddr(host); {
		case err == nil:
			if wildcard {
				return "", errors.New("domain wildcards cannot match an IP address")
			}
			host = ip.String()
		case host == "*" && !wildcard:
		default:
			host, err = idna.Lookup.ToASCII(host)
			host = strings.TrimRight(host, ".")
			last := host[strings.LastIndexByte(host, '.')+1:]
			_, numberErr := strconv.ParseUint(last, 10, 32)
			if err != nil || host == "" || strings.HasPrefix(host, ".") || strings.Contains(host, "..") || numberErr == nil || strings.HasPrefix(last, "0x") {
				return "", errors.New("invalid domain or noncanonical IPv4 address")
			}
		}
	}
	if wildcard {
		host = "[*.]" + host
	}
	if hasPort && port != "*" {
		n, err := strconv.ParseUint(port, 10, 16)
		if err != nil || strconv.FormatUint(n, 10) != port {
			return "", errors.New("port must be * or a decimal port without leading zeros")
		}
		host += ":" + port
	}
	if scheme != "*" {
		host = scheme + "://" + host
	}
	return host, nil
}

func isCookieException(entry any, setting int) bool {
	values, ok := entry.(map[string]any)
	if !ok {
		return false
	}
	return contentSettingInt(values[cookieExceptionSettingKey]) == setting
}

func (setting CookieSetting) valid() bool {
	_, valid := setting.contentSettingValue()
	return valid
}

func (setting CookieSetting) contentSettingValue() (int, bool) {
	value, valid := cookieSettingValues[setting]
	return value, valid
}

func (setting CookieSetting) contentSettingValueOrZero() int {
	value, _ := setting.contentSettingValue()
	return value
}

func (policy ThirdPartyCookiePolicy) valid() bool {
	if policy == "" {
		return true
	}
	_, valid := policy.mode()
	return valid
}

func (policy ThirdPartyCookiePolicy) mode() (int, bool) {
	mode, valid := thirdPartyCookieModes[policy]
	return mode, valid
}

func contentSettingInt(value any) int {
	switch value := value.(type) {
	case int:
		return value
	case int64:
		return int(value)
	case float64:
		return int(value)
	case json.Number:
		number, err := value.Int64()
		if err == nil {
			return int(number)
		}
	}
	return 0
}
