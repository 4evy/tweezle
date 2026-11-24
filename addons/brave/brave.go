// Package brave contributes Brave preferences and managed policies to Chromium.
// Config implements chromium.Provider for typed callers; Addon registers the
// opt-in [browser.brave] TOML table with chromium.Addon
package brave

import "github.com/4evy/tweezle/addons/chromium"

// Addon registers this browser's opt-in TOML section with a configuration
// loader
func Addon() chromium.ProviderAddon {
	return chromium.NewProviderAddon[Config]("brave")
}

// PolicyNamespace keeps Brave policies separate from generic Chromium policies
func (Config) PolicyNamespace() string { return "brave" }
