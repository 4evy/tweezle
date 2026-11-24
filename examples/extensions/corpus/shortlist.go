package main

import (
	"bytes"
	"cmp"
	"fmt"
	"maps"
	"os"
	"path/filepath"
	"regexp"

	repojson "github.com/4evy/tweezle/internal/json"
)

var commitID = regexp.MustCompile(`^[0-9a-f]{40}$`)

func (r *researcher) shortlist() error {
	poolFile := filepath.Join(r.cache, "pool.json")
	data, err := os.ReadFile(poolFile)
	if err != nil {
		return err
	}
	var poolData record
	if err := repojson.Default.DecodeInto(bytes.NewReader(data), &poolData); err != nil {
		return fmt.Errorf("%s: %w", poolFile, err)
	}
	type review struct {
		ID         string `json:"id"`
		Sample     string `json:"sample"`
		Repository string `json:"repository"`
		Commit     string `json:"commit"`
	}
	var reviews struct {
		Reviews []review `json:"reviews"`
	}
	if err := readJSON(filepath.Join(r.root, "reviews.json"), &reviews); err != nil {
		return err
	}
	samples := map[string]review{}
	for _, item := range reviews.Reviews {
		if _, exists := samples[item.ID]; exists {
			return fmt.Errorf("reviews.json: duplicated extension ID %s", item.ID)
		}
		samples[item.ID] = item
	}
	list, ok := poolData["extensions"].([]any)
	if !ok {
		return fmt.Errorf("pool.json: missing extensions array")
	}
	candidates := map[string]record{}
	inspected := 0
	for index, raw := range list {
		item, ok := raw.(map[string]any)
		if !ok {
			return fmt.Errorf("pool.json: extension %d is not an object", index)
		}
		id, ok := item["id"].(string)
		if !ok || candidates[id] != nil {
			return fmt.Errorf("pool.json: missing or duplicated extension ID %v", item["id"])
		}
		candidates[id] = item
		if evidence, ok := item["inspection"].(map[string]any); ok && evidence["status"] == "inspected" {
			inspected++
		}
	}
	retained := make([]record, 0, len(research.Selections))
	for _, chosen := range research.Selections {
		candidate, exists := candidates[chosen.ID]
		if !exists {
			return fmt.Errorf("pool.json: selected extension %s is missing", chosen.ID)
		}
		item := maps.Clone(candidate)
		sample, ok := samples[chosen.ID]
		if !ok || sample.Sample == "" || sample.Repository != chosen.Repository || !commitID.MatchString(sample.Commit) {
			return fmt.Errorf("selected extension %s has no matching source-pinned review", chosen.ID)
		}
		links := make([]string, 0, len(chosen.Files))
		for _, file := range chosen.Files {
			links = append(links, sample.Repository+"/blob/"+sample.Commit+"/"+file)
		}
		if inspection, ok := item["inspection"].(map[string]any); ok && inspection["status"] == "inspected" {
			inspection = maps.Clone(inspection)
			inspection["configuration_review"] = "Source-pinned configurable declarations reviewed; see reviews.json for coverage and exclusions"
			item["inspection"] = inspection
		}
		item["selection"] = record{
			"reasons": chosen.Reasons, "repository": chosen.Repository,
			"commit": sample.Commit, "source_urls": links, "sample": sample.Sample,
			"configuration_audit": record{
				"status":     "Source-declared configurable fields covered",
				"review":     "reviews.json",
				"scope":      "Storage defaults and configurable nested schemas; source-declared optional fields, table columns, builder cases, text-format commands, style schemas, and managed policies where applicable",
				"exclusions": "Action buttons, runtime caches and histories, generated databases and rules, server-owned identity and publishing state, other platforms, and premium-only features; per-case limitations are in reviews.json",
			},
			"live_browser_validation": cmp.Or(chosen.LiveBrowserValidation,
				"Not run; the Go fixture checks native storage persistence"),
		}
		retained = append(retained, item)
	}
	provenance := record{
		"collected_at":        poolData["collected_at"],
		"candidate_count":     poolData["candidate_count"],
		"valid_listing_count": poolData["valid_listing_count"],
		"packages_inspected":  inspected, "pool_size": len(list),
		"method": poolData["method"], "limitations": poolData["limitations"],
		"snapshot_sha256": hash(data), "cache_file": ".sources/extension-research/pool.json",
	}
	for _, key := range []string{"collection_started", "seed"} {
		if value, ok := poolData[key]; ok {
			provenance[key] = value
		}
	}
	output := record{
		"research_pool":     provenance,
		"selection_rule":    "Retain source-confirmed complex schemas, compressed/chunked values, migrations, policy precedence, recursive rules, or multiple persistence mechanisms; API marker counts alone are insufficient",
		"discarded_samples": []record{{"name": "Vimium", "reason": "Mostly flat settings; lower configuration complexity than the retained cases"}},
		"retained_count":    len(retained), "extensions": retained,
	}
	if err := writeJSON(filepath.Join(r.root, "corpus.json"), output); err != nil {
		return err
	}
	fmt.Printf("Retained %d complex extensions from %d inspected packages\n", len(retained), inspected)
	return nil
}
