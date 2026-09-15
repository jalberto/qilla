// Package prices imports per-token model prices from the LiteLLM price sheet
// (the same data getbifrost.ai/datasheet renders). Hive keeps $/MTok.
package prices

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

// DefaultURL is the upstream JSON.
const DefaultURL = "https://raw.githubusercontent.com/BerriAI/litellm/main/model_prices_and_context_window.json"

// Price per million tokens. cache_write_1h is what Claude Code pays: it uses
// 1-hour ephemeral caching (transcript usage.cache_creation.ephemeral_1h_input_tokens).
type Price struct {
	Input        float64 `json:"input"`
	Output       float64 `json:"output"`
	CacheRead    float64 `json:"cache_read"`
	CacheWrite   float64 `json:"cache_write"`
	CacheWrite1h float64 `json:"cache_write_1h,omitempty"`
}

type entry struct {
	Provider string  `json:"litellm_provider"`
	In       float64 `json:"input_cost_per_token"`
	Out      float64 `json:"output_cost_per_token"`
	Read     float64 `json:"cache_read_input_token_cost"`
	Write    float64 `json:"cache_creation_input_token_cost"`
	Write1h  float64 `json:"cache_creation_input_token_cost_above_1hr"`
}

// Parse keeps plain-id entries (no "provider/" prefix) of the given provider.
func Parse(b []byte, provider string) (map[string]Price, error) {
	var raw map[string]json.RawMessage
	if err := json.Unmarshal(b, &raw); err != nil {
		return nil, err
	}
	out := map[string]Price{}
	for id, v := range raw {
		if strings.Contains(id, "/") || id == "sample_spec" {
			continue
		}
		var e entry
		if json.Unmarshal(v, &e) != nil || e.Provider != provider || e.In == 0 {
			continue
		}
		p := Price{Input: mtok(e.In), Output: mtok(e.Out), CacheRead: mtok(e.Read), CacheWrite: mtok(e.Write), CacheWrite1h: mtok(e.Write1h)}
		if p.CacheWrite1h == 0 {
			p.CacheWrite1h = p.CacheWrite
		}
		out[id] = p
	}
	return out, nil
}

// mtok: $/token → $/MTok, rounded to 4 decimals (kills float noise like 0.19999999).
func mtok(perToken float64) float64 { return math.Round(perToken*1e6*1e4) / 1e4 }

// Fetch downloads and parses url.
func Fetch(url, provider string) (map[string]Price, error) {
	c := &http.Client{Timeout: 30 * time.Second}
	r, err := c.Get(url)
	if err != nil {
		return nil, err
	}
	defer r.Body.Close()
	if r.StatusCode != 200 {
		return nil, fmt.Errorf("%s: HTTP %d", url, r.StatusCode)
	}
	b, err := io.ReadAll(io.LimitReader(r.Body, 64<<20))
	if err != nil {
		return nil, err
	}
	return Parse(b, provider)
}

// Sorted ids.
func Sorted(m map[string]Price) []string {
	ids := make([]string, 0, len(m))
	for k := range m {
		ids = append(ids, k)
	}
	sort.Strings(ids)
	return ids
}

// File is where a synced sheet lives next to qilla.toml.
func File(configPath string) string { return filepath.Join(filepath.Dir(configPath), "prices.json") }

// Load reads a synced sheet. Missing file → empty map, nil error (unpriced).
func Load(path string) (map[string]Price, error) {
	b, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return map[string]Price{}, nil
	}
	if err != nil {
		return nil, err
	}
	var m map[string]Price
	return m, json.Unmarshal(b, &m)
}

// Save writes the sheet.
func Save(path string, m map[string]Price) error {
	b, err := json.MarshalIndent(m, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(path, b, 0o644)
}
