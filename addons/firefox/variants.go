package firefox

import (
	"bytes"
	_ "embed"
	"fmt"
	"slices"

	jsonutil "github.com/4evy/tweezle/internal/json"
)

type variant struct {
	ID            string `json:"id"`
	Name          string `json:"name"`
	LinuxLauncher string `json:"linux_launcher"`
	MacApp        string `json:"mac_app"`
	MacLauncher   string `json:"mac_launcher"`
}

// variants.json defines bundled identities in the public Variants order
//
//go:embed variants.json
var embeddedVariants []byte

// Presets provide executable identities only. Profile paths remain explicit
// because Firefox uses random profile names and forks can change their roots
// Unknown variants use the same engine with explicitly configured app paths
var variants, variantNames = loadVariants()

func loadVariants() (map[string]variant, []string) {
	catalog, err := jsonutil.Strict.Decode[[]variant](bytes.NewReader(embeddedVariants))
	if err != nil {
		panic(fmt.Errorf("decode Firefox variants: %w", err))
	}
	presets := make(map[string]variant, len(catalog))
	names := make([]string, len(catalog))
	for index, preset := range catalog {
		presets[preset.ID] = preset
		names[index] = preset.ID
	}
	return presets, names
}

func (config Config) defaults() Config {
	if config.Variant == "" && len(config.Providers) == 1 {
		config.Variant = config.Providers[0].Variant()
	}
	if config.Variant == "" {
		config.Variant = "firefox"
	}
	preset, known := variants[config.Variant]
	if config.Name == "" {
		config.Name = preset.Name
		if !known {
			config.Name = config.Variant
		}
	}
	if config.ExecutableName == "" {
		config.ExecutableName = config.Variant + "-configured"
	}
	if config.Linux.LauncherName == "" {
		config.Linux.LauncherName = preset.LinuxLauncher
	}
	if config.MacOS.AppDir == "" && known {
		config.MacOS.AppDir = "/Applications/" + preset.MacApp + ".app"
	}
	if config.MacOS.LauncherPath == "" && known {
		config.MacOS.LauncherPath = "Contents/MacOS/" + preset.MacLauncher
	}
	return config
}

// Variants lists the bundled identity presets. Explicit paths also support
// unlisted Gecko forks without requiring a new provider or engine
func Variants() []string {
	return slices.Clone(variantNames)
}
