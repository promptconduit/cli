package cost

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"time"
)

const (
	// PublishedPricingURL is the public rate card. A plain GET; no user data.
	PublishedPricingURL = "https://promptconduit.dev/model-rates.json"

	publishedSource = "promptconduit"
	cardFileName    = "pricing-card.json"
	cardCheckName   = "pricing-card-check.json"

	// PublishedCardTTL is how long a downloaded card is treated as fresh.
	PublishedCardTTL = 24 * time.Hour

	minPublishedModels = 10
)

// PublishedCardPath is the on-disk PromptConduit rate card
// (~/.config/promptconduit/cost/pricing-card.json).
func PublishedCardPath() string {
	return filepath.Join(StoreDir(), cardFileName)
}

func publishedCardCheckPath() string {
	return filepath.Join(StoreDir(), cardCheckName)
}

// PublishedCardStale reports whether the daily card download should run.
func PublishedCardStale(ttl time.Duration) bool {
	data, err := os.ReadFile(publishedCardCheckPath())
	if err != nil {
		return true
	}
	var stamp struct {
		CheckedAt time.Time `json:"checked_at"`
	}
	if json.Unmarshal(data, &stamp) != nil || stamp.CheckedAt.IsZero() {
		return true
	}
	return time.Since(stamp.CheckedAt) > ttl
}

// MarkPublishedCardChecked records that a download was started, so a burst of
// commands does not start a download every time.
func MarkPublishedCardChecked() error {
	dir := StoreDir()
	if dir == "" {
		return errors.New("cost: could not resolve config dir")
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	body, err := json.Marshal(struct {
		CheckedAt time.Time `json:"checked_at"`
	}{CheckedAt: time.Now()})
	if err != nil {
		return err
	}
	return writeAtomic(publishedCardCheckPath(), body)
}

// RefreshPublishedCard downloads the public rate card and stores it locally.
// A bad response leaves any previous card untouched.
func RefreshPublishedCard(ctx context.Context, url string) (int, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return 0, err
	}
	req.Header.Set("Accept", "application/json")
	req.Header.Set("User-Agent", "promptconduit")

	client := &http.Client{Timeout: 20 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		return 0, fmt.Errorf("fetch rate card: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		return 0, fmt.Errorf("fetch rate card: HTTP %d", resp.StatusCode)
	}
	data, err := io.ReadAll(io.LimitReader(resp.Body, 8*1024*1024))
	if err != nil {
		return 0, fmt.Errorf("read rate card: %w", err)
	}
	n, err := validatePublishedCard(data)
	if err != nil {
		return 0, err
	}
	if err := os.MkdirAll(filepath.Dir(PublishedCardPath()), 0o755); err != nil {
		return 0, err
	}
	if err := writeAtomic(PublishedCardPath(), data); err != nil {
		return 0, err
	}
	return n, nil
}

func validatePublishedCard(data []byte) (int, error) {
	models, meta, err := parsePriceDocument(data)
	if err != nil {
		return 0, fmt.Errorf("rate card did not parse: %w", err)
	}
	if meta.Source != publishedSource {
		return 0, fmt.Errorf("rate card _source = %q, want %q", meta.Source, publishedSource)
	}
	if len(meta.Updated) != len("2006-01-02") {
		return 0, fmt.Errorf("rate card _updated = %q, want YYYY-MM-DD", meta.Updated)
	}
	if _, err := time.Parse("2006-01-02", meta.Updated); err != nil {
		return 0, fmt.Errorf("rate card _updated: %w", err)
	}
	n := 0
	for _, mp := range models {
		if mp.Input == 0 && mp.Output == 0 {
			continue
		}
		n++
	}
	if n < minPublishedModels {
		return 0, fmt.Errorf("rate card has %d priced models, want at least %d", n, minPublishedModels)
	}
	return n, nil
}

func writeAtomic(path string, data []byte) error {
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, data, 0o644); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}
