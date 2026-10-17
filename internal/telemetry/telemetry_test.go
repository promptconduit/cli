package telemetry

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"regexp"
	"testing"
	"time"
)

func clearOptOutEnv(t *testing.T) {
	t.Helper()
	for _, k := range []string{"DO_NOT_TRACK", EnvTelemetry, "CI"} {
		t.Setenv(k, "")
	}
}

func TestEnabled(t *testing.T) {
	cases := []struct {
		name     string
		env      map[string]string
		disabled bool
		version  string
		want     bool
	}{
		{name: "default on", version: "1.2.3", want: true},
		{name: "config opt-out", version: "1.2.3", disabled: true, want: false},
		{name: "dev build", version: "dev", want: false},
		{name: "empty version", version: "", want: false},
		{name: "DO_NOT_TRACK=1", version: "1.2.3", env: map[string]string{"DO_NOT_TRACK": "1"}, want: false},
		{name: "DO_NOT_TRACK=true", version: "1.2.3", env: map[string]string{"DO_NOT_TRACK": "true"}, want: false},
		{name: "DO_NOT_TRACK=0", version: "1.2.3", env: map[string]string{"DO_NOT_TRACK": "0"}, want: true},
		{name: "PROMPTCONDUIT_TELEMETRY=0", version: "1.2.3", env: map[string]string{EnvTelemetry: "0"}, want: false},
		{name: "PROMPTCONDUIT_TELEMETRY=off", version: "1.2.3", env: map[string]string{EnvTelemetry: "off"}, want: false},
		{name: "PROMPTCONDUIT_TELEMETRY=1", version: "1.2.3", env: map[string]string{EnvTelemetry: "1"}, want: true},
		{name: "CI=true", version: "1.2.3", env: map[string]string{"CI": "true"}, want: false},
		{name: "CI=false", version: "1.2.3", env: map[string]string{"CI": "false"}, want: true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			clearOptOutEnv(t)
			for k, v := range tc.env {
				t.Setenv(k, v)
			}
			if got := Enabled(tc.disabled, tc.version); got != tc.want {
				t.Fatalf("Enabled(%v, %q) = %v, want %v", tc.disabled, tc.version, got, tc.want)
			}
		})
	}
}

func withNow(t *testing.T, ts time.Time) {
	t.Helper()
	prev := now
	now = func() time.Time { return ts }
	t.Cleanup(func() { now = prev })
}

func TestDue(t *testing.T) {
	base := time.Date(2026, 10, 17, 12, 0, 0, 0, time.UTC)
	withNow(t, base)
	shown := base.Add(-48 * time.Hour)

	cases := []struct {
		name string
		st   State
		want bool
	}{
		{name: "notice not shown", st: State{}, want: false},
		{name: "first ping after notice", st: State{NoticeShownAt: shown}, want: true},
		{name: "already sent today", st: State{NoticeShownAt: shown, LastSentDay: "2026-10-17"}, want: false},
		{name: "sent yesterday", st: State{NoticeShownAt: shown, LastSentDay: "2026-10-16"}, want: true},
		{name: "attempt within retry window", st: State{NoticeShownAt: shown, LastAttemptAt: base.Add(-10 * time.Minute)}, want: false},
		{name: "attempt past retry window", st: State{NoticeShownAt: shown, LastAttemptAt: base.Add(-RetryAfter)}, want: true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := tc.st.Due(); got != tc.want {
				t.Fatalf("Due() = %v, want %v", got, tc.want)
			}
		})
	}
}

func TestMarkSentUsesUTCDay(t *testing.T) {
	// 23:30 in UTC-5 is already the next day in UTC.
	withNow(t, time.Date(2026, 10, 17, 23, 30, 0, 0, time.FixedZone("EST", -5*3600)))
	st := &State{}
	st.MarkSent()
	if st.LastSentDay != "2026-10-18" {
		t.Fatalf("LastSentDay = %q, want UTC day 2026-10-18", st.LastSentDay)
	}
}

var uuidV4 = regexp.MustCompile(`^[0-9a-f]{8}-[0-9a-f]{4}-4[0-9a-f]{3}-[89ab][0-9a-f]{3}-[0-9a-f]{12}$`)

func TestSaveLoadAssignsStableInstallID(t *testing.T) {
	dir := t.TempDir()
	if st := Load(dir); st.InstallID != "" {
		t.Fatalf("missing file should load empty, got %+v", st)
	}

	st := &State{}
	st.MarkNoticeShown()
	if err := Save(dir, st); err != nil {
		t.Fatal(err)
	}
	if !uuidV4.MatchString(st.InstallID) {
		t.Fatalf("install ID %q is not a v4 UUID", st.InstallID)
	}

	again := Load(dir)
	if again.InstallID != st.InstallID || again.NoticeShownAt.IsZero() {
		t.Fatalf("round trip lost data: %+v", again)
	}
	if err := Save(dir, again); err != nil {
		t.Fatal(err)
	}
	if Load(dir).InstallID != st.InstallID {
		t.Fatal("install ID changed on re-save")
	}
}

func TestLoadCorruptFileYieldsEmptyState(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(Path(dir), []byte("{not json"), 0o644); err != nil {
		t.Fatal(err)
	}
	if st := Load(dir); st.InstallID != "" || !st.NoticeShownAt.IsZero() {
		t.Fatalf("corrupt file should load empty, got %+v", st)
	}
}

func TestSendPostsPayload(t *testing.T) {
	var got Payload
	var path, ua, ctype string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		path, ua, ctype = r.URL.Path, r.UserAgent(), r.Header.Get("Content-Type")
		_ = json.NewDecoder(r.Body).Decode(&got)
		w.WriteHeader(http.StatusNoContent)
	}))
	defer srv.Close()

	p := Payload{InstallID: "id", CLIVersion: "1.2.3", OS: "darwin", Arch: "arm64", Tools: []string{"cursor"}, LoggedIn: true}
	if err := Send(context.Background(), nil, srv.URL+"/", "promptconduit/1.2.3", p); err != nil {
		t.Fatal(err)
	}
	if path != "/v1/ping" || ua != "promptconduit/1.2.3" || ctype != "application/json" {
		t.Fatalf("unexpected request: path=%q ua=%q content-type=%q", path, ua, ctype)
	}
	if got.InstallID != "id" || got.CLIVersion != "1.2.3" || len(got.Tools) != 1 || !got.LoggedIn {
		t.Fatalf("unexpected payload: %+v", got)
	}
}

func TestSendTreatsHTTPErrorsAsDelivered(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusBadRequest)
	}))
	defer srv.Close()
	if err := Send(context.Background(), nil, srv.URL, "ua", Payload{}); err != nil {
		t.Fatalf("a 400 should not be retried, got %v", err)
	}
}

func TestSendReturnsTransportErrors(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	url := srv.URL
	srv.Close()
	if err := Send(context.Background(), nil, url, "ua", Payload{}); err == nil {
		t.Fatal("expected a transport error for a closed server")
	}
}
