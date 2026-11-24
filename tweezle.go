// Package tweezle loads explicitly registered addons and runs their operations.
// Engines implement Module and optionally BrowserModule and PolicyRenderer.
// Applications choose their engines and product providers explicitly
package tweezle

import (
	"bytes"
	"context"
	jsonv1 "encoding/json"
	json "encoding/json/v2"
	"errors"
	"fmt"
	"math/big"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"

	jsonutil "github.com/4evy/tweezle/internal/json"
	"github.com/4evy/tweezle/internal/reflectutil"
	"github.com/4evy/tweezle/launcher"
	"github.com/pelletier/go-toml/v2"
)

// Source identifies the configuration file for addon-relative paths; Path and
// Directory are absolute when loading a file
type Source struct {
	Path      string
	Directory string
}

// Module implements an addon's validated configuration and operations. Run
// returns false for operations it does not recognize. Addons handle common
// operations and may define additional operation types
type Module interface {
	Validate() error
	Run(context.Context, any) (bool, error)
}

// Addon owns explicit top-level TOML tables. The decoder receives only those
// tables, keeping unknown fields visible. Browser-family addons can compose
// their own more specialized providers
type Addon struct {
	Name     string
	Sections []string
	Decode   func([]byte, Source) (Module, error)
}

// Binding names a module for validation and operation errors
type Binding struct {
	Name   string
	Module Module
}

// Config holds modules in execution order. Applications may construct it
// directly without a configuration file
type Config struct {
	Modules []Binding
}

var namePattern = regexp.MustCompile(`^[a-z][a-z0-9_]*$`)

// LoadConfig reads and validates TOML using only the supplied addons. Decoders
// receive an absolute Source for resolving configuration-relative paths.
// Loading does not execute module operations
func LoadConfig(path string, addons ...Addon) (Config, error) {
	path, err := filepath.Abs(path)
	if err != nil {
		return Config{}, err
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return Config{}, fmt.Errorf("read config %s: %w", path, err)
	}
	config, err := DecodeConfig(data, Source{Path: path, Directory: filepath.Dir(path)}, addons...)
	if err != nil {
		return Config{}, fmt.Errorf("config %s: %w", path, err)
	}
	return config, nil
}

// DecodeConfig rejects unowned sections before invoking any addon decoder.
// Addons must strictly decode their own tables and reject unknown fields
func DecodeConfig(data []byte, source Source, addons ...Addon) (Config, error) {
	owners := map[string]string{}
	names := map[string]bool{}
	for _, addon := range addons {
		if !namePattern.MatchString(addon.Name) {
			return Config{}, fmt.Errorf("invalid addon name %q", addon.Name)
		}
		if names[addon.Name] {
			return Config{}, fmt.Errorf("addon %q is registered more than once", addon.Name)
		}
		if addon.Decode == nil || len(addon.Sections) == 0 {
			return Config{}, fmt.Errorf("addon %q requires sections and a decoder", addon.Name)
		}
		names[addon.Name] = true
		for _, section := range addon.Sections {
			if !namePattern.MatchString(section) {
				return Config{}, fmt.Errorf("invalid addon section %q", section)
			}
			if owner, exists := owners[section]; exists {
				return Config{}, fmt.Errorf("section %q is owned by both %q and %q", section, owner, addon.Name)
			}
			owners[section] = addon.Name
		}
	}
	var document map[string]any
	if err := toml.Unmarshal(data, &document); err != nil {
		return Config{}, err
	}
	var unknown []string
	for section := range document {
		if _, exists := owners[section]; !exists {
			unknown = append(unknown, section)
		}
	}
	if len(unknown) > 0 {
		slices.Sort(unknown)
		return Config{}, fmt.Errorf("unregistered configuration sections: %s", strings.Join(unknown, ", "))
	}
	var config Config
	for _, addon := range addons {
		tables := map[string]any{}
		for _, section := range addon.Sections {
			if value, exists := document[section]; exists {
				tables[section] = value
			}
		}
		if len(tables) == 0 {
			continue
		}
		encoded, err := toml.Marshal(tables)
		if err != nil {
			return Config{}, fmt.Errorf("addon %s: %w", addon.Name, err)
		}
		module, err := addon.Decode(encoded, source)
		if err != nil {
			return Config{}, fmt.Errorf("addon %s: %w", addon.Name, err)
		}
		config.Modules = append(config.Modules, Binding{Name: addon.Name, Module: module})
	}
	if err := config.Validate(); err != nil {
		return Config{}, err
	}
	return config, nil
}

// Validate collects module validation errors without running operations
func (config Config) Validate() error {
	var errs []error
	names := map[string]bool{}
	for _, binding := range config.Modules {
		if !namePattern.MatchString(binding.Name) || names[binding.Name] {
			errs = append(errs, fmt.Errorf("invalid or duplicate module name %q", binding.Name))
		}
		names[binding.Name] = true
		if reflectutil.IsNil(binding.Module) {
			errs = append(errs, fmt.Errorf("addon %s returned no module", binding.Name))
			continue
		}
		if err := binding.Module.Validate(); err != nil {
			errs = append(errs, fmt.Errorf("addon %s: %w", binding.Name, err))
		}
	}
	return errors.Join(errs...)
}

// Run validates all modules before executing any of them, in registration
// order; errors stop subsequent modules and completed operations are not rolled
// back
func (config Config) Run(ctx context.Context, operation any) error {
	if err := config.Validate(); err != nil {
		return err
	}
	handled := false
	for _, binding := range config.Modules {
		if err := ctx.Err(); err != nil {
			return err
		}
		accepted, err := binding.Module.Run(ctx, operation)
		if err != nil {
			return fmt.Errorf("addon %s: %w", binding.Name, err)
		}
		handled = handled || accepted
	}
	if !handled {
		return fmt.Errorf("no registered addon handles operation %T", operation)
	}
	return nil
}

// MergePolicies combines process-wide intent and rejects conflicting values
func MergePolicies(documents ...map[string]any) (map[string]any, error) {
	merged := map[string]any{}
	for index, document := range documents {
		for name, value := range document {
			if existing, ok := merged[name]; ok {
				equal, err := equalPolicy(existing, value)
				if err != nil {
					return nil, fmt.Errorf("compare managed policy %q in configuration %d: %w", name, index+1, err)
				}
				if !equal {
					return nil, fmt.Errorf("managed policy %q conflicts in configuration %d: %#v and %#v", name, index+1, existing, value)
				}
			}
			merged[name] = value
		}
	}
	return merged, nil
}

func equalPolicy(left, right any) (bool, error) {
	a, err := json.Marshal(left, json.Deterministic(true))
	if err != nil {
		return false, err
	}
	b, err := json.Marshal(right, json.Deterministic(true))
	if err != nil {
		return false, err
	}
	if bytes.Equal(a, b) {
		return true, nil
	}
	l, err := jsonutil.Default.Decode[any](bytes.NewReader(a))
	if err != nil {
		return false, err
	}
	r, err := jsonutil.Default.Decode[any](bytes.NewReader(b))
	if err != nil {
		return false, err
	}
	return equalPolicyJSON(l, r), nil
}

func equalPolicyJSON(left, right any) bool {
	switch left := left.(type) {
	case map[string]any:
		right, ok := right.(map[string]any)
		if !ok || len(left) != len(right) {
			return false
		}
		for key, x := range left {
			if y, exists := right[key]; !exists || !equalPolicyJSON(x, y) {
				return false
			}
		}
		return true
	case []any:
		right, ok := right.([]any)
		if !ok || len(left) != len(right) {
			return false
		}
		for i, x := range left {
			if !equalPolicyJSON(x, right[i]) {
				return false
			}
		}
		return true
	case jsonv1.Number:
		right, ok := right.(jsonv1.Number)
		return ok && (left == right || equalPolicyNumber(left.String(), right.String()))
	default:
		// JSON scalars other than numbers are directly comparable
		return left == right
	}
}

func equalPolicyNumber(a, b string) bool {
	left, leftExp, leftShift, leftNeg := policyNumberParts(a)
	right, rightExp, rightShift, rightNeg := policyNumberParts(b)
	if left != right {
		return false
	}
	if left == "" {
		return true
	}
	if leftNeg != rightNeg {
		return false
	}
	if leftExp == rightExp {
		return leftShift == rightShift
	}
	// Compare exponents without expanding powers of ten or rounding
	var l, r, shift big.Int
	l.SetString(leftExp, 10)
	r.SetString(rightExp, 10)
	l.Add(&l, shift.SetInt64(int64(leftShift)))
	r.Add(&r, shift.SetInt64(int64(rightShift)))
	return l.Cmp(&r) == 0
}

// policyNumberParts separates a valid JSON number into its significant digits
// and decimal exponent, expressed as the written exponent plus a shift
func policyNumberParts(value string) (digits, exponent string, shift int, negative bool) {
	value, negative = strings.CutPrefix(value, "-")
	exponent = "0"
	if i := strings.IndexAny(value, "eE"); i >= 0 {
		value, exponent = value[:i], value[i+1:]
	}
	whole, fraction, _ := strings.Cut(value, ".")
	fraction = strings.TrimRight(fraction, "0")
	if fraction != "" {
		return strings.TrimLeft(whole+fraction, "0"), exponent, -len(fraction), negative
	}
	digits = strings.TrimRight(whole, "0")
	return digits, exponent, len(whole) - len(digits), negative
}

// RunLauncher detects and executes an installed browser launcher
func RunLauncher(invocation string, arguments []string) (bool, error) {
	return launcher.Run(invocation, arguments)
}
