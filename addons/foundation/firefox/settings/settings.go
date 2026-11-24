// Package settings validates source-backed fork preference catalogs
package settings

import (
	"bytes"
	json "encoding/json/v2"
	"errors"
	"fmt"
	"maps"
	"math"
	"strconv"

	"github.com/4evy/tweezle/addons/foundation/firefox"
	jsonutil "github.com/4evy/tweezle/internal/json"
	"github.com/santhosh-tekuri/jsonschema/v6"
)

// Definition records a native preference type and its source location
// JSON preferences use Schema to validate partial declarative patches
type Definition struct {
	Type    string  `json:"type"`
	Source  string  `json:"source"`
	Schema  *Schema `json:"schema,omitempty"`
	Default any     `json:"default,omitempty"`
	Locked  bool    `json:"locked,omitempty"`
}

type Catalog struct {
	Revision    string                `json:"revision"`
	Repository  string                `json:"repository"`
	Preferences map[string]Definition `json:"preferences"`
}

// Schema is a JSON Schema document for partial preference patches
type Schema map[string]any

// CompiledSchema validates values against an immutable compiled schema
type CompiledSchema struct {
	schema *jsonschema.Schema
}

// CompiledCatalog holds decoded definitions and reusable validators without
// exposing its mutable metadata
type CompiledCatalog struct {
	catalog Catalog
	byName  map[string]*CompiledSchema
	byType  map[string]*CompiledSchema
}

func Load(data []byte) Catalog {
	var catalog Catalog
	if err := json.Unmarshal(data, &catalog); err != nil {
		panic(fmt.Errorf("decode embedded fork metadata: %w", err))
	}
	return catalog
}

// LoadCompiled loads immutable embedded metadata and compiles its schemas once
// Invalid embedded metadata is a programming error, as with Load
func LoadCompiled(data []byte) *CompiledCatalog {
	catalog := &CompiledCatalog{
		catalog: Load(data),
		byName:  map[string]*CompiledSchema{},
		byType:  map[string]*CompiledSchema{},
	}
	for name, definition := range catalog.catalog.Preferences {
		if definition.Schema == nil && catalog.byType[definition.Type] != nil {
			continue
		}
		compiled, err := definition.schema().Compile()
		if err != nil {
			panic(fmt.Errorf("compile embedded fork preference %s schema: %w", name, err))
		}
		if definition.Schema == nil {
			catalog.byType[definition.Type] = compiled
		} else {
			catalog.byName[name] = compiled
		}
	}
	return catalog
}

// Lookup reports whether the named preference is locked and present
func (catalog *CompiledCatalog) Lookup(name string) (locked, present bool) {
	definition, present := catalog.catalog.Preferences[name]
	return definition.Locked, present
}

func (catalog *CompiledCatalog) Validate(values map[string]any) error {
	_, err := catalog.Values(firefox.Profile{}, values)
	return err
}

func (catalog *CompiledCatalog) Values(profile firefox.Profile, values map[string]any) (map[string]any, error) {
	return catalog.catalog.values(profile, values, catalog)
}

func (definition Definition) schema() Schema {
	if definition.Schema != nil {
		return *definition.Schema
	}
	typ := definition.Type
	if typ == "decimal" {
		typ = "number"
	}
	schema := Schema{}
	if typ != "any" {
		schema["type"] = typ
	}
	if typ == "integer" {
		schema["minimum"], schema["maximum"] = math.MinInt32, math.MaxInt32
	}
	return schema
}

func (catalog Catalog) Validate(values map[string]any) error {
	_, err := catalog.Values(firefox.Profile{}, values)
	return err
}

func (catalog Catalog) Values(profile firefox.Profile, values map[string]any) (map[string]any, error) {
	return catalog.values(profile, values, nil)
}

func (catalog Catalog) values(profile firefox.Profile, values map[string]any, cache *CompiledCatalog) (map[string]any, error) {
	result := map[string]any{}
	var errs []error
	for name, value := range values {
		definition, ok := catalog.Preferences[name]
		if !ok {
			errs = append(errs, fmt.Errorf("unknown fork preference %q; use firefox.preferences for uncatalogued preferences", name))
			continue
		}
		var schema *CompiledSchema
		if cache != nil {
			if definition.Schema == nil {
				schema = cache.byType[definition.Type]
			} else {
				schema = cache.byName[name]
			}
		} else {
			var err error
			schema, err = definition.schema().Compile()
			if err != nil {
				errs = append(errs, fmt.Errorf("%s schema: %w", name, err))
				continue
			}
		}
		normalized, err := schema.validatedValue(value, name)
		if err != nil {
			errs = append(errs, err)
			continue
		}
		if definition.Type == "json" {
			input := profile
			if _, present := profile.Preferences[name]; !present && definition.Default != nil {
				input.Preferences = maps.Clone(profile.Preferences)
				if input.Preferences == nil {
					input.Preferences = map[string]any{}
				}
				encoded, err := json.Marshal(definition.Default)
				if err != nil {
					errs = append(errs, err)
					continue
				}
				input.Preferences[name] = string(encoded)
			}
			text, err := firefox.MergeJSONPreference(input, name, normalized)
			if err != nil {
				errs = append(errs, err)
				continue
			}
			result[name] = text
		} else if definition.Type == "decimal" {
			encoded, err := json.Marshal(value)
			if err != nil {
				errs = append(errs, err)
				continue
			}
			number, err := strconv.ParseFloat(string(encoded), 64)
			if err != nil {
				errs = append(errs, err)
				continue
			}
			result[name] = strconv.FormatFloat(number, 'f', -1, 64)
		} else {
			result[name] = value
		}
	}
	return result, errors.Join(errs...)
}

func normalize(value any) (any, error) {
	data, err := json.Marshal(value)
	if err != nil {
		return nil, err
	}
	return jsonutil.Default.Decode[any](bytes.NewReader(data))
}

// Compile prepares a schema for repeated validation
func (schema Schema) Compile() (*CompiledSchema, error) {
	compiler := jsonschema.NewCompiler()
	compiler.DefaultDraft(jsonschema.Draft2020)
	const resource = "urn:browser:fork-preference"
	document, err := normalize(schema)
	if err != nil {
		return nil, err
	}
	if err := compiler.AddResource(resource, document); err != nil {
		return nil, err
	}
	compiled, err := compiler.Compile(resource)
	if err != nil {
		return nil, err
	}
	return &CompiledSchema{schema: compiled}, nil
}

func (schema Schema) Validate(value any, path string) error {
	compiled, err := schema.Compile()
	if err != nil {
		return fmt.Errorf("%s schema: %w", path, err)
	}
	return compiled.Validate(value, path)
}

func (schema *CompiledSchema) Validate(value any, path string) error {
	_, err := schema.validatedValue(value, path)
	return err
}

func (schema *CompiledSchema) validatedValue(value any, path string) (any, error) {
	value, err := normalize(value)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	if err := schema.schema.Validate(value); err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	return value, nil
}
