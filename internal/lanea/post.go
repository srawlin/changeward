package lanea

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"strings"
	"time"
)

var httpClient = &http.Client{Timeout: 30 * time.Second}

func doJSON(method, u, token, authHeader string, body, out any) error {
	var rd io.Reader
	if body != nil {
		b, _ := json.Marshal(body)
		rd = bytes.NewReader(b)
	}
	req, err := http.NewRequest(method, u, rd)
	if err != nil {
		return err
	}
	if authHeader == "" {
		req.Header.Set("Authorization", "Bearer "+token)
	} else {
		req.Header.Set(authHeader, token)
	}
	req.Header.Set("Accept", "application/json")
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := httpClient.Do(req)
	if err != nil {
		return err
	}
	defer func() { _ = resp.Body.Close() }()
	b, _ := io.ReadAll(resp.Body)
	if resp.StatusCode >= 300 {
		return fmt.Errorf("%s %s: %s: %s", method, u, resp.Status, bytes.TrimSpace(b))
	}
	if out != nil {
		return json.Unmarshal(b, out)
	}
	return nil
}

// GitHub posts reports and statuses to a GitHub pull request.
type GitHub struct {
	API   string // default https://api.github.com or $GITHUB_API_URL
	Repo  string // owner/name
	PR    int
	SHA   string
	Token string
}

// GitHubFromEnv reads GitHub Actions environment variables.
func GitHubFromEnv() (*GitHub, error) {
	g := &GitHub{API: os.Getenv("GITHUB_API_URL"), Repo: os.Getenv("GITHUB_REPOSITORY"), Token: os.Getenv("GITHUB_TOKEN"), SHA: os.Getenv("GITHUB_SHA")}
	if g.API == "" {
		g.API = "https://api.github.com"
	}
	if ev := os.Getenv("GITHUB_EVENT_PATH"); ev != "" {
		if b, err := os.ReadFile(ev); err == nil {
			var e struct {
				Number      int `json:"number"`
				PullRequest struct {
					Number int `json:"number"`
					Head   struct {
						SHA string `json:"sha"`
					} `json:"head"`
				} `json:"pull_request"`
			}
			if json.Unmarshal(b, &e) == nil {
				g.PR = e.PullRequest.Number
				if g.PR == 0 {
					g.PR = e.Number
				}
				if e.PullRequest.Head.SHA != "" {
					g.SHA = e.PullRequest.Head.SHA
				}
			}
		}
	}
	if g.Token == "" || g.Repo == "" {
		return nil, errors.New("github: GITHUB_TOKEN and GITHUB_REPOSITORY are required")
	}
	return g, nil
}

// Comment creates or updates the Changeward comment on the PR.
func (g *GitHub) Comment(body string) (string, error) { return g.CommentWithMarker(body, Marker) }

// CommentWithMarker creates or updates the comment containing marker.
func (g *GitHub) CommentWithMarker(body, marker string) (string, error) {
	if g.PR == 0 {
		return "", errors.New("github: not a pull request (no PR number)")
	}
	var comments []struct {
		ID      int64  `json:"id"`
		Body    string `json:"body"`
		HTMLURL string `json:"html_url"`
	}
	base := fmt.Sprintf("%s/repos/%s/issues/%d/comments", g.API, g.Repo, g.PR)
	if err := doJSON("GET", base+"?per_page=100", g.Token, "", nil, &comments); err != nil {
		return "", err
	}
	var out struct {
		HTMLURL string `json:"html_url"`
	}
	for _, c := range comments {
		if strings.Contains(c.Body, marker) {
			err := doJSON("PATCH", fmt.Sprintf("%s/repos/%s/issues/comments/%d", g.API, g.Repo, c.ID), g.Token, "", map[string]string{"body": body}, &out)
			return out.HTMLURL, err
		}
	}
	err := doJSON("POST", base, g.Token, "", map[string]string{"body": body}, &out)
	return out.HTMLURL, err
}

// Status sets a commit status ("success", "failure", "pending", "error").
func (g *GitHub) Status(state, description, targetURL string) error {
	if g.SHA == "" {
		return errors.New("github: no commit SHA")
	}
	if len(description) > 140 {
		description = description[:137] + "..."
	}
	return doJSON("POST", fmt.Sprintf("%s/repos/%s/statuses/%s", g.API, g.Repo, g.SHA), g.Token, "",
		map[string]string{"state": state, "description": description, "context": "changeward/rehearsal", "target_url": targetURL}, nil)
}

// GitLab posts reports to a GitLab merge request.
type GitLab struct {
	API     string // $CI_API_V4_URL
	Project string // $CI_PROJECT_ID
	MR      string // $CI_MERGE_REQUEST_IID
	SHA     string
	Token   string // $CHANGEWARD_GITLAB_TOKEN or $GITLAB_TOKEN (api scope)
}

// GitLabFromEnv reads GitLab CI environment variables.
func GitLabFromEnv() (*GitLab, error) {
	g := &GitLab{API: os.Getenv("CI_API_V4_URL"), Project: os.Getenv("CI_PROJECT_ID"), MR: os.Getenv("CI_MERGE_REQUEST_IID"),
		SHA: os.Getenv("CI_COMMIT_SHA"), Token: os.Getenv("CHANGEWARD_GITLAB_TOKEN")}
	if g.Token == "" {
		g.Token = os.Getenv("GITLAB_TOKEN")
	}
	if g.API == "" || g.Project == "" || g.Token == "" {
		return nil, errors.New("gitlab: CI_API_V4_URL, CI_PROJECT_ID and CHANGEWARD_GITLAB_TOKEN (api scope) are required")
	}
	return g, nil
}

// Comment creates or updates the Changeward note on the MR.
func (g *GitLab) Comment(body string) error {
	if g.MR == "" {
		return errors.New("gitlab: not a merge request pipeline")
	}
	base := fmt.Sprintf("%s/projects/%s/merge_requests/%s/notes", g.API, url.PathEscape(g.Project), g.MR)
	var notes []struct {
		ID   int64  `json:"id"`
		Body string `json:"body"`
	}
	if err := doJSON("GET", base+"?per_page=100&sort=desc", g.Token, "PRIVATE-TOKEN", nil, &notes); err != nil {
		return err
	}
	for _, n := range notes {
		if strings.Contains(n.Body, Marker) {
			return doJSON("PUT", fmt.Sprintf("%s/%d", base, n.ID), g.Token, "PRIVATE-TOKEN", map[string]string{"body": body}, nil)
		}
	}
	return doJSON("POST", base, g.Token, "PRIVATE-TOKEN", map[string]string{"body": body}, nil)
}

// Status sets a commit status ("success", "failed", "pending").
func (g *GitLab) Status(state, description string) error {
	if g.SHA == "" {
		return errors.New("gitlab: no commit SHA")
	}
	q := url.Values{"state": {state}, "name": {"changeward/rehearsal"}, "description": {description}}
	return doJSON("POST", fmt.Sprintf("%s/projects/%s/statuses/%s?%s", g.API, url.PathEscape(g.Project), g.SHA, q.Encode()), g.Token, "PRIVATE-TOKEN", nil, nil)
}
