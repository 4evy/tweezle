package main

import (
	"bytes"
	_ "embed"
	"fmt"

	repojson "github.com/4evy/tweezle/internal/json"
)

// research.json holds discovery inputs and shortlist evidence in source order
// Rebuild the collector after editing it, generated corpus.json is output only
//
//go:embed research.json
var researchJSON []byte

var research = loadResearch()

type researchCatalog struct {
	Categories   []string `json:"categories"`
	Supplemental []struct {
		ID   string `json:"id"`
		Name string `json:"name"`
	} `json:"supplemental"`
	Selections []selection `json:"selections"`
}

type selection struct {
	Repo                  string   `json:"repo"`
	Repository            string   `json:"repository"`
	ID                    string   `json:"id"`
	Files                 []string `json:"files"`
	Reasons               []string `json:"reasons"`
	LiveBrowserValidation string   `json:"live_browser_validation"`
}

func loadResearch() researchCatalog {
	catalog, err := repojson.Strict.Decode[researchCatalog](bytes.NewReader(researchJSON))
	if err != nil {
		panic(fmt.Errorf("decode extension research catalog: %w", err))
	}
	return catalog
}
