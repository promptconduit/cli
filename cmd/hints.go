package cmd

import (
	"fmt"
	"net/http"
	"regexp"
	"strconv"
	"time"

	"github.com/promptconduit/cli/internal/client"
)

// connectionHint is the next step for a failed API call, by HTTP status (0 =
// no response at all). Empty when there is nothing useful to add.
func connectionHint(status int, apiURL string) string {
	switch {
	case status == http.StatusUnauthorized || status == http.StatusForbidden:
		return "Your API key was rejected (revoked, expired, or issued for a different environment).\n" +
			"Sign in again with: promptconduit login"
	case status == 0:
		return fmt.Sprintf("Could not reach %s. Check your network connection, and the API URL with: promptconduit config show", apiURL)
	case status == http.StatusTooManyRequests:
		return "Rate limited. Wait a minute and try again."
	case status >= 500:
		return "The PromptConduit API had a server error. Try again in a few minutes; captured events are kept locally and replayed automatically."
	}
	return ""
}

// apiErrorMessage is the most readable part of a failed reply: the problem
// detail (or title) from an RFC 7807 body when there is one, else the raw
// error text.
func apiErrorMessage(resp *client.APIResponse) string {
	if resp.Data != nil {
		if d, ok := resp.Data["detail"].(string); ok && d != "" {
			return fmt.Sprintf("HTTP %d: %s", resp.StatusCode, d)
		}
		if t, ok := resp.Data["title"].(string); ok && t != "" {
			return fmt.Sprintf("HTTP %d: %s", resp.StatusCode, t)
		}
	}
	return resp.Error
}

// sendErrorStatus matches the status of a recorded event-send failure
// ("API error: 401 - {...}", see the client's attemptEventSend).
var sendErrorStatus = regexp.MustCompile(`^API error: (\d{3}) `)

// authFailureHint returns a sign-in hint when the most recent event send
// failed on auth and nothing has been delivered since, i.e. every send is
// still being rejected. Empty otherwise: an old auth error that later sends
// got past needs no action.
func authFailureHint(lastError, lastErrorAt, lastSuccessAt string) string {
	m := sendErrorStatus.FindStringSubmatch(lastError)
	if m == nil {
		return ""
	}
	status, _ := strconv.Atoi(m[1])
	if status != http.StatusUnauthorized && status != http.StatusForbidden {
		return ""
	}
	errAt, err := time.Parse(time.RFC3339, lastErrorAt)
	if err != nil {
		return ""
	}
	if okAt, err := time.Parse(time.RFC3339, lastSuccessAt); err == nil && !okAt.Before(errAt) {
		return ""
	}
	return "Events are being rejected: your API key is no longer accepted. Sign in again with: promptconduit login"
}
