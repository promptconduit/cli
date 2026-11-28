package ci

import (
	"bufio"
	"encoding/json"
	"io"
	"os"
	"strings"
)

// Session is what the local event log says about one agent session in a repo.
type Session struct {
	ID       string
	First    string
	Last     string
	TokenUSD float64
	PRs      map[int]bool
}

// SessionIndex maps commits and PRs of one repo to the sessions that had them.
type SessionIndex struct {
	BySHA    map[string]map[string]bool
	ByPR     map[int]map[string]bool
	Sessions map[string]*Session
}

type indexEnvelope struct {
	SessionID   string `json:"session_id"`
	CapturedAt  string `json:"captured_at"`
	Enrichments struct {
		VCS *struct {
			Repo   string `json:"repo"`
			Commit *struct {
				Hash string `json:"hash"`
			} `json:"commit"`
			PR *struct {
				Number int `json:"number"`
			} `json:"pr"`
		} `json:"vcs"`
		Cost *struct {
			Requests []struct {
				RequestID   string `json:"request_id"`
				ModelPriced bool   `json:"model_priced"`
				USD         struct {
					Total float64 `json:"total"`
				} `json:"usd"`
			} `json:"requests"`
		} `json:"cost"`
	} `json:"enrichments"`
}

// IndexSessions reads the event log at path for one repo ("owner/name").
// Every hook event records HEAD, so a session that pushed commit X has an
// event with HEAD = X. Token $ sums each priced request once.
func IndexSessions(path, repo string) (*SessionIndex, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer func() { _ = f.Close() }()
	return indexSessions(f, repo)
}

func indexSessions(r io.Reader, repo string) (*SessionIndex, error) {
	idx := &SessionIndex{BySHA: map[string]map[string]bool{}, ByPR: map[int]map[string]bool{}, Sessions: map[string]*Session{}}
	want := strings.ToLower(repo)
	// Cheap pre-filter: skip lines that don't mention the repo at all.
	needle := []byte(strings.ToLower(`"` + repo + `"`))
	seenReq := map[string]bool{}
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 1024*1024), 64*1024*1024)
	for sc.Scan() {
		line := sc.Bytes()
		if !containsFold(line, needle) {
			continue
		}
		var e indexEnvelope
		if json.Unmarshal(line, &e) != nil || e.SessionID == "" {
			continue
		}
		v := e.Enrichments.VCS
		if v == nil || strings.ToLower(v.Repo) != want {
			continue
		}
		s := idx.Sessions[e.SessionID]
		if s == nil {
			s = &Session{ID: e.SessionID, First: e.CapturedAt, Last: e.CapturedAt, PRs: map[int]bool{}}
			idx.Sessions[e.SessionID] = s
		}
		if e.CapturedAt != "" && (s.First == "" || e.CapturedAt < s.First) {
			s.First = e.CapturedAt
		}
		if e.CapturedAt > s.Last {
			s.Last = e.CapturedAt
		}
		if v.Commit != nil && v.Commit.Hash != "" {
			h := strings.ToLower(v.Commit.Hash)
			if idx.BySHA[h] == nil {
				idx.BySHA[h] = map[string]bool{}
			}
			idx.BySHA[h][e.SessionID] = true
		}
		if v.PR != nil && v.PR.Number > 0 {
			s.PRs[v.PR.Number] = true
			if idx.ByPR[v.PR.Number] == nil {
				idx.ByPR[v.PR.Number] = map[string]bool{}
			}
			idx.ByPR[v.PR.Number][e.SessionID] = true
		}
		if c := e.Enrichments.Cost; c != nil {
			for _, req := range c.Requests {
				if req.RequestID == "" || seenReq[req.RequestID] || !req.ModelPriced {
					continue
				}
				seenReq[req.RequestID] = true
				s.TokenUSD += req.USD.Total
			}
		}
	}
	return idx, sc.Err()
}

// containsFold reports whether needle (lowercase) occurs in s, ignoring ASCII case.
func containsFold(s, needle []byte) bool {
	n := len(needle)
	for i := 0; i+n <= len(s); i++ {
		match := true
		for k := 0; k < n; k++ {
			c := s[i+k]
			if 'A' <= c && c <= 'Z' {
				c += 'a' - 'A'
			}
			if c != needle[k] {
				match = false
				break
			}
		}
		if match {
			return true
		}
	}
	return false
}
