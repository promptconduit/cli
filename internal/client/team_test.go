package client

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
)

type recordedRequest struct {
	Method, Path, Query, Auth string
	Body                      map[string]interface{}
}

func newTeamTestClient(t *testing.T, status int, reply string) (*Client, *recordedRequest) {
	t.Helper()
	rec := &recordedRequest{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		rec.Method, rec.Path, rec.Query = r.Method, r.URL.Path, r.URL.RawQuery
		rec.Auth = r.Header.Get("Authorization")
		if b, _ := io.ReadAll(r.Body); len(b) > 0 {
			_ = json.Unmarshal(b, &rec.Body)
		}
		w.WriteHeader(status)
		_, _ = io.WriteString(w, reply)
	}))
	t.Cleanup(srv.Close)
	return NewClient(&Config{APIURL: srv.URL, APIKey: "k", TimeoutSeconds: 5}, "test"), rec
}

func TestTeamEndpoints(t *testing.T) {
	cases := []struct {
		name         string
		call         func(c *Client) *APIResponse
		method, path string
		query        string
		body         map[string]interface{}
	}{
		{"orgs", func(c *Client) *APIResponse { return c.ListOrganizations() }, "GET", "/v1/organizations", "", nil},
		{"members", func(c *Client) *APIResponse { return c.ListOrgMembers("o1") }, "GET", "/v1/organizations/o1/members", "", nil},
		{"cost", func(c *Client) *APIResponse { return c.GetTeamCost("o1", "7d") }, "GET", "/v1/organizations/o1/analytics/cost", "period=7d", nil},
		{"savings", func(c *Client) *APIResponse { return c.GetTeamSavings("o1", "90d") }, "GET", "/v1/organizations/o1/analytics/savings", "period=90d", nil},
		{"budgets", func(c *Client) *APIResponse { return c.ListBudgets("o1") }, "GET", "/v1/organizations/o1/budgets", "", nil},
		{"set budget", func(c *Client) *APIResponse { return c.SetBudget("o1", "repo", "acme/api", 500) },
			"POST", "/v1/organizations/o1/budgets", "", map[string]interface{}{"scope": "repo", "scopeValue": "acme/api", "monthlyUsd": 500.0}},
		{"rm budget", func(c *Client) *APIResponse { return c.DeleteBudget("o1", "b1") }, "DELETE", "/v1/organizations/o1/budgets/b1", "", nil},
		{"create", func(c *Client) *APIResponse { return c.CreateOrganization("Acme", "", true) },
			"POST", "/v1/organizations", "", map[string]interface{}{"name": "Acme", "attachHistory": true}},
		{"invite", func(c *Client) *APIResponse { return c.InviteMember("o1", "a@b.co", "admin") },
			"POST", "/v1/organizations/o1/invites", "", map[string]interface{}{"email": "a@b.co", "role": "admin"}},
		{"accept", func(c *Client) *APIResponse { return c.AcceptInvite("tok") }, "POST", "/v1/organizations/invites/tok/accept", "", nil},
		{"attach", func(c *Client) *APIResponse { return c.AttachHistory("o1") }, "POST", "/v1/organizations/o1/attach-history", "", nil},
		{"tag", func(c *Client) *APIResponse { return c.SetRepoClient("o1", "acme/api", "Acme") },
			"PUT", "/v1/organizations/o1/repo-clients", "", map[string]interface{}{"repoName": "acme/api", "client": "Acme"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			c, rec := newTeamTestClient(t, 200, `{"ok":true}`)
			resp := tc.call(c)
			if !resp.Success || resp.Data["ok"] != true {
				t.Fatalf("resp = %+v", resp)
			}
			if rec.Method != tc.method || rec.Path != tc.path || rec.Query != tc.query {
				t.Errorf("got %s %s?%s, want %s %s?%s", rec.Method, rec.Path, rec.Query, tc.method, tc.path, tc.query)
			}
			if rec.Auth != "Bearer k" {
				t.Errorf("auth = %q", rec.Auth)
			}
			for k, v := range tc.body {
				if rec.Body[k] != v {
					t.Errorf("body[%s] = %v, want %v", k, rec.Body[k], v)
				}
			}
		})
	}
}

func TestExportTeamCost(t *testing.T) {
	c, rec := newTeamTestClient(t, 200, "client,spend_usd\nAcme,12.5\n")
	csv, fail := c.ExportTeamCost("o1", "30d", "client")
	if fail != nil {
		t.Fatal(fail.Error)
	}
	if csv != "client,spend_usd\nAcme,12.5\n" {
		t.Errorf("csv = %q", csv)
	}
	if rec.Path != "/v1/organizations/o1/analytics/export" || rec.Query != "by=client&period=30d" {
		t.Errorf("got %s?%s", rec.Path, rec.Query)
	}
}

func TestExportTeamCostError(t *testing.T) {
	c, _ := newTeamTestClient(t, 403, `{"detail":"upgrade"}`)
	_, fail := c.ExportTeamCost("o1", "30d", "repo")
	if fail == nil || fail.StatusCode != 403 || fail.Data["detail"] != "upgrade" {
		t.Fatalf("fail = %+v", fail)
	}
}

func TestGetEncodesQuery(t *testing.T) {
	c, rec := newTeamTestClient(t, 200, `{}`)
	c.GetTeamCost("o1", "7d&x=1#frag")
	if rec.Query != "period=7d%26x%3D1%23frag" {
		t.Errorf("query = %q", rec.Query)
	}
}
