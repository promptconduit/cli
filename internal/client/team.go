package client

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"time"
)

// Team (organization) endpoints. These are the same reads and writes the web
// dashboard's team page uses, authenticated with the CLI's API key, so an
// agent can run team spend governance without opening the dashboard.

func orgPath(orgID, suffix string) string {
	return "/v1/organizations/" + url.PathEscape(orgID) + suffix
}

// ListOrganizations returns the organizations the user belongs to.
func (c *Client) ListOrganizations() *APIResponse {
	return c.Get("/v1/organizations", nil)
}

// CreateOrganization creates a team with the caller as owner. attachHistory
// adds the owner's last 30 days of usage to the new team.
func (c *Client) CreateOrganization(name, slug string, attachHistory bool) *APIResponse {
	body := map[string]interface{}{"name": name, "attachHistory": attachHistory}
	if slug != "" {
		body["slug"] = slug
	}
	return c.jsonRequest("POST", "/v1/organizations", body)
}

// InviteMember creates an invite; the reply carries the invite URL to share.
func (c *Client) InviteMember(orgID, email, role string) *APIResponse {
	return c.jsonRequest("POST", orgPath(orgID, "/invites"), map[string]string{"email": email, "role": role})
}

// AcceptInvite joins the team an invite token belongs to.
func (c *Client) AcceptInvite(token string) *APIResponse {
	return c.jsonRequest("POST", "/v1/organizations/invites/"+url.PathEscape(token)+"/accept", map[string]string{})
}

// AttachHistory shares the caller's last 30 days of usage with a team.
func (c *Client) AttachHistory(orgID string) *APIResponse {
	return c.jsonRequest("POST", orgPath(orgID, "/attach-history"), map[string]string{})
}

// ListOrgMembers returns an organization's active members.
func (c *Client) ListOrgMembers(orgID string) *APIResponse {
	return c.Get(orgPath(orgID, "/members"), nil)
}

// GetTeamCost returns team spend totals and breakdowns for a period (7d|30d|90d).
func (c *Client) GetTeamCost(orgID, period string) *APIResponse {
	return c.Get(orgPath(orgID, "/analytics/cost"), map[string]string{"period": period})
}

// GetTeamSavings returns the team savings report for a period.
func (c *Client) GetTeamSavings(orgID, period string) *APIResponse {
	return c.Get(orgPath(orgID, "/analytics/savings"), map[string]string{"period": period})
}

// ListBudgets returns the organization's monthly budgets with month-to-date spend.
func (c *Client) ListBudgets(orgID string) *APIResponse {
	return c.Get(orgPath(orgID, "/budgets"), nil)
}

// SetBudget creates or updates the monthly budget for a scope (org, repo, member).
// scopeValue is the repo name or member user id; empty for org.
func (c *Client) SetBudget(orgID, scope, scopeValue string, monthlyUSD float64) *APIResponse {
	return c.jsonRequest("POST", orgPath(orgID, "/budgets"), map[string]interface{}{
		"scope":      scope,
		"scopeValue": scopeValue,
		"monthlyUsd": monthlyUSD,
	})
}

// DeleteBudget removes a budget by id.
func (c *Client) DeleteBudget(orgID, budgetID string) *APIResponse {
	return c.deleteRequest(orgPath(orgID, "/budgets/"+url.PathEscape(budgetID)))
}

// SetRepoClient tags a repo with a client; an empty client clears the tag.
func (c *Client) SetRepoClient(orgID, repoName, clientName string) *APIResponse {
	return c.jsonRequest("PUT", orgPath(orgID, "/repo-clients"), map[string]string{
		"repoName": repoName,
		"client":   clientName,
	})
}

// ExportTeamCost returns team spend as CSV, grouped by client|repo|member|pr|day.
func (c *Client) ExportTeamCost(orgID, period, by string) (string, error) {
	ctx, cancel := context.WithTimeout(context.Background(), time.Duration(c.config.TimeoutSeconds)*time.Second)
	defer cancel()

	q := url.Values{"period": {period}, "by": {by}}
	req, err := http.NewRequestWithContext(ctx, "GET", c.config.APIURL+orgPath(orgID, "/analytics/export")+"?"+q.Encode(), nil)
	if err != nil {
		return "", fmt.Errorf("failed to create request: %w", err)
	}
	c.setHeaders(req)

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return "", fmt.Errorf("request failed: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return "", fmt.Errorf("failed to read response: %w", err)
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return "", &HTTPError{StatusCode: resp.StatusCode, Body: string(body)}
	}
	return string(body), nil
}

// HTTPError is a non-2xx reply whose body could not be parsed into an APIResponse.
type HTTPError struct {
	StatusCode int
	Body       string
}

func (e *HTTPError) Error() string {
	return fmt.Sprintf("HTTP %d: %s", e.StatusCode, e.Body)
}
