// Package telemetry sends one anonymous usage ping per install per day so the
// project can count active installs. The payload is deliberately coarse: a
// random install ID generated on this machine, the CLI version, OS, CPU
// architecture, which tool hooks are installed, the editor extension version,
// and whether the user is signed in. It never includes names, emails, paths,
// repository names, prompts, or costs.
//
// Opt out with `promptconduit config set --disable-telemetry=true`,
// DO_NOT_TRACK=1, or PROMPTCONDUIT_TELEMETRY=0. It is also off when CI is set
// and for dev builds. Nothing is sent until an interactive command has shown
// the one-time notice (see Notice).
package telemetry

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"github.com/google/uuid"
)

const (
	// EnvTelemetry set to 0/false/off/no disables the ping.
	EnvTelemetry = "PROMPTCONDUIT_TELEMETRY"

	// StateFileName lives in the config dir (~/.config/promptconduit).
	StateFileName = "telemetry.json"

	// RetryAfter is the minimum gap between attempts, so a failed send (no
	// network) doesn't respawn a sender on every hook.
	RetryAfter = time.Hour

	sendTimeout = 10 * time.Second
	pingPath    = "/v1/ping"
)

// Notice is printed once, on an interactive command, before anything is sent.
// It names every field in Payload.
const Notice = "promptconduit: sends one anonymous usage ping per day: a random install ID, CLI version, OS and " +
	"CPU architecture, which tool hooks and editor extension version are installed, and whether you're signed in. " +
	"No prompts, paths, repos, or personal data. Opt out: promptconduit config set --disable-telemetry=true " +
	"(details: https://github.com/promptconduit/cli#anonymous-usage-ping)"

// releaseVersion matches tagged release builds only. Local `make` builds
// (git describe: v1.2.3-4-gabc123-dirty), snapshots (1.2.4-dev), and plain
// `go build` ("dev") never ping.
var releaseVersion = regexp.MustCompile(`^v?\d+\.\d+\.\d+$`)

// now is swapped in tests.
var now = time.Now

// State is the content of the telemetry state file.
type State struct {
	InstallID     string    `json:"install_id"`
	NoticeShownAt time.Time `json:"notice_shown_at,omitempty"`
	LastSentDay   string    `json:"last_sent_day,omitempty"` // UTC YYYY-MM-DD
	LastAttemptAt time.Time `json:"last_attempt_at,omitempty"`
}

// Payload is the JSON body of POST /v1/ping.
type Payload struct {
	InstallID        string   `json:"install_id"`
	CLIVersion       string   `json:"cli_version"`
	OS               string   `json:"os"`
	Arch             string   `json:"arch"`
	Tools            []string `json:"tools"`
	ExtensionVersion string   `json:"extension_version,omitempty"`
	LoggedIn         bool     `json:"logged_in"`
}

// Enabled reports whether telemetry may run, given the config opt-out and the
// running version. Environment opt-outs are read here. Only release builds
// are eligible.
func Enabled(configDisabled bool, version string) bool {
	if configDisabled || !releaseVersion.MatchString(version) {
		return false
	}
	if v := os.Getenv("DO_NOT_TRACK"); v != "" && !isFalsy(v) {
		return false
	}
	if v := os.Getenv(EnvTelemetry); v != "" && isFalsy(v) {
		return false
	}
	if v := os.Getenv("CI"); v != "" && !isFalsy(v) {
		return false
	}
	return true
}

func isFalsy(v string) bool {
	switch strings.ToLower(strings.TrimSpace(v)) {
	case "0", "false", "off", "no":
		return true
	}
	return false
}

// Path returns the state file path inside dir.
func Path(dir string) string { return filepath.Join(dir, StateFileName) }

// Load reads the state file. A missing or unreadable file yields an empty
// state; the install ID is assigned on first Save.
func Load(dir string) *State {
	st := &State{}
	data, err := os.ReadFile(Path(dir))
	if err != nil {
		return st
	}
	if json.Unmarshal(data, st) != nil {
		return &State{}
	}
	return st
}

// Save writes the state atomically, assigning an install ID if missing.
func Save(dir string, st *State) error {
	if st.InstallID == "" {
		id, err := uuid.NewRandom()
		if err != nil {
			return err
		}
		st.InstallID = id.String()
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	data, err := json.MarshalIndent(st, "", "  ")
	if err != nil {
		return err
	}
	tmp, err := os.CreateTemp(dir, ".telemetry-*.json")
	if err != nil {
		return err
	}
	if _, err := tmp.Write(data); err != nil {
		_ = tmp.Close()
		_ = os.Remove(tmp.Name())
		return err
	}
	if err := tmp.Close(); err != nil {
		_ = os.Remove(tmp.Name())
		return err
	}
	if err := os.Rename(tmp.Name(), Path(dir)); err != nil {
		// Windows refuses to replace a file another process has open; drop
		// the temp file rather than leaving it behind.
		_ = os.Remove(tmp.Name())
		return err
	}
	return nil
}

// today is the current UTC day, matching the server's bucketing.
func today() string { return now().UTC().Format("2006-01-02") }

// Due reports whether a ping should be attempted now: the notice has been
// shown, nothing has been sent today, and the last attempt is over RetryAfter
// old. Callers check Enabled first.
func (st *State) Due() bool {
	if st.NoticeShownAt.IsZero() || st.LastSentDay == today() {
		return false
	}
	return st.LastAttemptAt.IsZero() || now().Sub(st.LastAttemptAt) >= RetryAfter
}

// MarkNoticeShown records that the user has seen the notice.
func (st *State) MarkNoticeShown() { st.NoticeShownAt = now().UTC() }

// MarkAttempt claims the retry window before a sender is spawned, so sibling
// hooks firing at the same moment don't each start one.
func (st *State) MarkAttempt() { st.LastAttemptAt = now().UTC() }

// MarkSent records a completed send for today.
func (st *State) MarkSent() { st.LastSentDay = today() }

// Send POSTs the payload to apiURL + /v1/ping. Any HTTP response counts as
// delivered (the server dedupes per day and a 4xx won't fix itself by
// retrying); only transport errors are returned so the caller retries later.
// A nil hc uses http.DefaultClient.
func Send(ctx context.Context, hc *http.Client, apiURL, userAgent string, p Payload) error {
	if hc == nil {
		hc = http.DefaultClient
	}
	body, err := json.Marshal(p)
	if err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(ctx, sendTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, strings.TrimRight(apiURL, "/")+pingPath, bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("User-Agent", userAgent)
	resp, err := hc.Do(req)
	if err != nil {
		return err
	}
	_ = resp.Body.Close()
	return nil
}
