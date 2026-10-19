package lanea

import (
	"fmt"
	"net/url"
	"sort"
	"strings"
)

// Approvals returns the logins whose latest review approves the PR's current
// head commit (approvals of older commits do not count), and the PR author.
func (g *GitHub) Approvals() (approvedBy []string, author string, err error) {
	if g.PR == 0 {
		return nil, "", fmt.Errorf("github: not a pull request (no PR number)")
	}
	var pr struct {
		User struct {
			Login string `json:"login"`
		} `json:"user"`
		Head struct {
			SHA string `json:"sha"`
		} `json:"head"`
	}
	if err := doJSON("GET", fmt.Sprintf("%s/repos/%s/pulls/%d", g.API, g.Repo, g.PR), g.Token, "", nil, &pr); err != nil {
		return nil, "", err
	}
	var reviews []struct {
		User struct {
			Login string `json:"login"`
		} `json:"user"`
		State    string `json:"state"`
		CommitID string `json:"commit_id"`
	}
	if err := doJSON("GET", fmt.Sprintf("%s/repos/%s/pulls/%d/reviews?per_page=100", g.API, g.Repo, g.PR), g.Token, "", nil, &reviews); err != nil {
		return nil, "", err
	}
	latest := map[string]string{} // login → state of their latest decisive review
	for _, r := range reviews {   // chronological
		switch r.State {
		case "APPROVED":
			if r.CommitID == pr.Head.SHA {
				latest[r.User.Login] = "APPROVED"
			} else {
				latest[r.User.Login] = "STALE"
			}
		case "CHANGES_REQUESTED", "DISMISSED":
			latest[r.User.Login] = r.State
		}
	}
	for login, st := range latest {
		if st == "APPROVED" {
			approvedBy = append(approvedBy, login)
		}
	}
	sort.Strings(approvedBy)
	return approvedBy, pr.User.Login, nil
}

// Approvals returns the usernames that approved the merge request, and its author.
func (g *GitLab) Approvals() (approvedBy []string, author string, err error) {
	if g.MR == "" {
		return nil, "", fmt.Errorf("gitlab: not a merge request pipeline")
	}
	base := fmt.Sprintf("%s/projects/%s/merge_requests/%s", g.API, url.PathEscape(g.Project), g.MR)
	var mr struct {
		Author struct {
			Username string `json:"username"`
		} `json:"author"`
	}
	if err := doJSON("GET", base, g.Token, "PRIVATE-TOKEN", nil, &mr); err != nil {
		return nil, "", err
	}
	var ap struct {
		ApprovedBy []struct {
			User struct {
				Username string `json:"username"`
			} `json:"user"`
		} `json:"approved_by"`
	}
	if err := doJSON("GET", base+"/approvals", g.Token, "PRIVATE-TOKEN", nil, &ap); err != nil {
		return nil, "", err
	}
	for _, a := range ap.ApprovedBy {
		approvedBy = append(approvedBy, a.User.Username)
	}
	sort.Strings(approvedBy)
	return approvedBy, mr.Author.Username, nil
}

// Satisfied reports whether code-review approvals meet a "review required"
// decision. The author's own approval never counts. Every `user:LOGIN`
// approver must have approved, and at least max(quorum, 1) distinct people.
// Other approver kinds (owner:codeowners, group:...) cannot be checked from a
// review list; they are returned in platformEnforced and should be enforced by
// the platform (CODEOWNERS with "Require review from Code Owners", GitLab
// approval rules).
func Satisfied(approvers []string, quorum int, approvedBy []string, author string) (ok bool, platformEnforced []string, why string) {
	people := map[string]bool{}
	for _, a := range approvedBy {
		if !strings.EqualFold(a, author) {
			people[strings.ToLower(a)] = true
		}
	}
	for _, a := range approvers {
		if login, isUser := strings.CutPrefix(a, "user:"); isUser {
			if !people[strings.ToLower(login)] {
				return false, nil, "waiting for " + a
			}
			continue
		}
		platformEnforced = append(platformEnforced, a)
	}
	if quorum < 1 {
		quorum = 1
	}
	if len(people) < quorum {
		return false, platformEnforced, fmt.Sprintf("%d of %d approval(s) from someone other than the author", len(people), quorum)
	}
	var names []string
	for p := range people {
		names = append(names, "@"+p)
	}
	sort.Strings(names)
	return true, platformEnforced, "approved by " + strings.Join(names, ", ")
}
