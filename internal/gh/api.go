package gh

import (
	"encoding/base64"
	"fmt"
	"net/url"
	"strings"
	"time"
)

// Repo is one repository of the org as seen by the app installation.
type Repo struct {
	Name          string
	Archived      bool
	DefaultBranch string
	DefaultHead   string // commit SHA of the default branch (empty for empty repos)
	DeployFile    *string
}

// ListRepos returns every repo the installation can see, including the
// content of .swarm/deploy.yml on the default branch (nil if absent).
func (c *Client) ListRepos() ([]Repo, error) {
	const q = `query($org:String!,$cursor:String){
  organization(login:$org){
    repositories(first:50, after:$cursor, orderBy:{field:NAME,direction:ASC}){
      pageInfo{hasNextPage endCursor}
      nodes{
        name isArchived isDisabled
        defaultBranchRef{ name target{ oid } }
        deploy: object(expression:"HEAD:.swarm/deploy.yml"){ ... on Blob{ text isTruncated } }
      }
    }
  }
}`
	var repos []Repo
	var cursor *string
	for {
		var out struct {
			Organization struct {
				Repositories struct {
					PageInfo struct {
						HasNextPage bool   `json:"hasNextPage"`
						EndCursor   string `json:"endCursor"`
					} `json:"pageInfo"`
					Nodes []struct {
						Name             string `json:"name"`
						IsArchived       bool   `json:"isArchived"`
						IsDisabled       bool   `json:"isDisabled"`
						DefaultBranchRef *struct {
							Name   string `json:"name"`
							Target struct {
								Oid string `json:"oid"`
							} `json:"target"`
						} `json:"defaultBranchRef"`
						Deploy *struct {
							Text        *string `json:"text"`
							IsTruncated bool    `json:"isTruncated"`
						} `json:"deploy"`
					} `json:"nodes"`
				} `json:"repositories"`
			} `json:"organization"`
		}
		if err := c.GraphQL(q, map[string]any{"org": c.Org, "cursor": cursor}, &out); err != nil {
			return nil, err
		}
		for _, n := range out.Organization.Repositories.Nodes {
			r := Repo{Name: n.Name, Archived: n.IsArchived || n.IsDisabled}
			if n.DefaultBranchRef != nil {
				r.DefaultBranch = n.DefaultBranchRef.Name
				r.DefaultHead = n.DefaultBranchRef.Target.Oid
			}
			if n.Deploy != nil && n.Deploy.Text != nil {
				if n.Deploy.IsTruncated {
					return nil, fmt.Errorf("%s: .swarm/deploy.yml too large", n.Name)
				}
				t := *n.Deploy.Text
				r.DeployFile = &t
			}
			repos = append(repos, r)
		}
		pi := out.Organization.Repositories.PageInfo
		if !pi.HasNextPage {
			return repos, nil
		}
		ec := pi.EndCursor
		cursor = &ec
	}
}

// Branch is a branch head with the data needed to decide what to deploy.
type Branch struct {
	Name       string
	Commit     string
	Message    string
	TreeOid    string
	ParentTree string // tree of the only parent; empty for root/merge commits
	SwarmTree  string // oid of the .swarm directory tree; empty if absent
}

// EmptyCommit reports whether the head commit changes nothing (git commit --allow-empty).
func (b Branch) EmptyCommit() bool { return b.ParentTree != "" && b.ParentTree == b.TreeOid }

// Branches lists all branches of a repo.
func (c *Client) Branches(repo string) ([]Branch, error) {
	const q = `query($org:String!,$name:String!,$cursor:String){
  repository(owner:$org,name:$name){
    refs(refPrefix:"refs/heads/", first:100, after:$cursor){
      pageInfo{hasNextPage endCursor}
      nodes{ name target{ ... on Commit{
        oid messageHeadline tree{ oid entries{ name type oid } }
        parents(first:1){ totalCount nodes{ tree{oid} } }
      } } }
    }
  }
}`
	var res []Branch
	var cursor *string
	for {
		var out struct {
			Repository *struct {
				Refs struct {
					PageInfo struct {
						HasNextPage bool   `json:"hasNextPage"`
						EndCursor   string `json:"endCursor"`
					} `json:"pageInfo"`
					Nodes []struct {
						Name   string `json:"name"`
						Target struct {
							Oid             string `json:"oid"`
							MessageHeadline string `json:"messageHeadline"`
							Tree            struct {
								Oid     string `json:"oid"`
								Entries []struct {
									Name string `json:"name"`
									Type string `json:"type"`
									Oid  string `json:"oid"`
								} `json:"entries"`
							} `json:"tree"`
							Parents struct {
								TotalCount int `json:"totalCount"`
								Nodes      []struct {
									Tree struct {
										Oid string `json:"oid"`
									} `json:"tree"`
								} `json:"nodes"`
							} `json:"parents"`
						} `json:"target"`
					} `json:"nodes"`
				} `json:"refs"`
			} `json:"repository"`
		}
		if err := c.GraphQL(q, map[string]any{"org": c.Org, "name": repo, "cursor": cursor}, &out); err != nil {
			return nil, err
		}
		if out.Repository == nil {
			return nil, fmt.Errorf("repository %s not found", repo)
		}
		for _, n := range out.Repository.Refs.Nodes {
			b := Branch{Name: n.Name, Commit: n.Target.Oid, Message: n.Target.MessageHeadline, TreeOid: n.Target.Tree.Oid}
			if n.Target.Parents.TotalCount == 1 && len(n.Target.Parents.Nodes) == 1 {
				b.ParentTree = n.Target.Parents.Nodes[0].Tree.Oid
			}
			// .swarm is looked up in the root tree's entries rather than with
			// file(path:".swarm"): GitHub answers file() on a branch without
			// .swarm with a NOT_FOUND error, not null, and GraphQL() fails the
			// whole call on any error - so a single feature branch without
			// .swarm used to block every branch of the repo.
			for _, e := range n.Target.Tree.Entries {
				if e.Name == ".swarm" && e.Type == "tree" {
					b.SwarmTree = e.Oid
				}
			}
			res = append(res, b)
		}
		pi := out.Repository.Refs.PageInfo
		if !pi.HasNextPage {
			return res, nil
		}
		ec := pi.EndCursor
		cursor = &ec
	}
}

// EnvProperties returns repo name -> allowed environments from an org custom
// property (single or multi select). Repos without a value are absent.
func (c *Client) EnvProperties(property string) (map[string][]string, error) {
	res := map[string][]string{}
	for page := 1; ; page++ {
		var out []struct {
			RepositoryName string `json:"repository_name"`
			Properties     []struct {
				Name  string `json:"property_name"`
				Value any    `json:"value"`
			} `json:"properties"`
		}
		if err := c.do("GET", fmt.Sprintf("/orgs/%s/properties/values?per_page=100&page=%d", c.Org, page), nil, &out); err != nil {
			return nil, err
		}
		for _, r := range out {
			for _, p := range r.Properties {
				if p.Name != property {
					continue
				}
				switch v := p.Value.(type) {
				case string:
					for _, s := range strings.Split(v, ",") {
						if s = strings.TrimSpace(s); s != "" {
							res[r.RepositoryName] = append(res[r.RepositoryName], s)
						}
					}
				case []any:
					for _, s := range v {
						if str, ok := s.(string); ok {
							res[r.RepositoryName] = append(res[r.RepositoryName], str)
						}
					}
				}
			}
		}
		if len(out) < 100 {
			return res, nil
		}
	}
}

// TreeEntry is one file inside the .swarm directory.
type TreeEntry struct {
	Path string `json:"path"`
	Mode string `json:"mode"`
	Type string `json:"type"`
	Sha  string `json:"sha"`
	Size int64  `json:"size"`
}

// Tree lists a tree recursively.
func (c *Client) Tree(repo, sha string) ([]TreeEntry, error) {
	var out struct {
		Tree      []TreeEntry `json:"tree"`
		Truncated bool        `json:"truncated"`
	}
	if err := c.do("GET", fmt.Sprintf("/repos/%s/%s/git/trees/%s?recursive=1", c.Org, repo, sha), nil, &out); err != nil {
		return nil, err
	}
	if out.Truncated {
		return nil, fmt.Errorf("tree %s too large", sha)
	}
	return out.Tree, nil
}

// Blob returns the content of a blob.
func (c *Client) Blob(repo, sha string) ([]byte, error) {
	var out struct {
		Content  string `json:"content"`
		Encoding string `json:"encoding"`
	}
	if err := c.do("GET", fmt.Sprintf("/repos/%s/%s/git/blobs/%s", c.Org, repo, sha), nil, &out); err != nil {
		return nil, err
	}
	if out.Encoding != "base64" {
		return []byte(out.Content), nil
	}
	return base64.StdEncoding.DecodeString(strings.ReplaceAll(out.Content, "\n", ""))
}

// BranchProtected reports whether changes to the branch must go through a
// pull request with at least one required approval: classic protection with
// required reviews, or a ruleset with a "pull_request" rule requiring
// approvals. Weaker protections alone (blocking force-pushes, requiring only
// a status check, requiring a pull request but no approval, ...) are not
// enough: a collaborator with push access could still land a commit on the
// branch directly, or self-merge an unreviewed pull request, which is
// exactly what REQUIRE_PROTECTED is meant to prevent.
func (c *Client) BranchProtected(repo, branch string) (bool, error) {
	var prot struct {
		RequiredPullRequestReviews *struct {
			RequiredApprovingReviewCount int `json:"required_approving_review_count"`
		} `json:"required_pull_request_reviews"`
	}
	err := c.do("GET", fmt.Sprintf("/repos/%s/%s/branches/%s/protection", c.Org, repo, url.PathEscape(branch)), nil, &prot)
	switch {
	case err == nil:
		if r := prot.RequiredPullRequestReviews; r != nil && r.RequiredApprovingReviewCount >= 1 {
			return true, nil
		}
	case !IsNotFound(err):
		return false, err
	}
	var rules []struct {
		Type       string `json:"type"`
		Parameters struct {
			RequiredApprovingReviewCount int `json:"required_approving_review_count"`
		} `json:"parameters"`
	}
	if err := c.do("GET", fmt.Sprintf("/repos/%s/%s/rules/branches/%s", c.Org, repo, url.PathEscape(branch)), nil, &rules); err != nil {
		return false, err
	}
	for _, r := range rules {
		if r.Type == "pull_request" && r.Parameters.RequiredApprovingReviewCount >= 1 {
			return true, nil
		}
	}
	return false, nil
}

// CheckRun is a created check run.
type CheckRun struct {
	ID      int64  `json:"id"`
	HTMLURL string `json:"html_url"`
}

// CreateCheckRun starts an in-progress check run on a commit.
func (c *Client) CreateCheckRun(repo, sha, name string) (*CheckRun, error) {
	var cr CheckRun
	err := c.do("POST", fmt.Sprintf("/repos/%s/%s/check-runs", c.Org, repo), map[string]any{
		"name": name, "head_sha": sha, "status": "in_progress", "started_at": time.Now().UTC().Format(time.RFC3339),
	}, &cr)
	return &cr, err
}

// CompleteCheckRun finishes a check run. Conclusion: success, failure, neutral, skipped.
func (c *Client) CompleteCheckRun(repo string, id int64, conclusion, title, summary, text string) error {
	return c.do("PATCH", fmt.Sprintf("/repos/%s/%s/check-runs/%d", c.Org, repo, id), map[string]any{
		"status": "completed", "conclusion": conclusion, "completed_at": time.Now().UTC().Format(time.RFC3339),
		"output": map[string]any{"title": truncate(title, 250), "summary": truncate(summary, 60000), "text": truncate(text, 60000)},
	}, nil)
}

// CreateDeployment records a deployment of sha to a GitHub environment.
func (c *Client) CreateDeployment(repo, sha, env, description string, production, transient bool) (int64, error) {
	var d struct {
		ID int64 `json:"id"`
	}
	err := c.do("POST", fmt.Sprintf("/repos/%s/%s/deployments", c.Org, repo), map[string]any{
		"ref": sha, "environment": env, "description": truncate(description, 140),
		"auto_merge": false, "required_contexts": []string{},
		"production_environment": production, "transient_environment": transient,
	}, &d)
	return d.ID, err
}

// DeploymentStatus sets the state of a deployment (in_progress, success, failure, error, inactive).
func (c *Client) DeploymentStatus(repo string, id int64, state, description, logURL, envURL string) error {
	body := map[string]any{"state": state, "description": truncate(description, 140), "auto_inactive": true}
	if logURL != "" {
		body["log_url"] = logURL
	}
	if envURL != "" {
		body["environment_url"] = envURL
	}
	return c.do("POST", fmt.Sprintf("/repos/%s/%s/deployments/%d/statuses", c.Org, repo, id), body, nil)
}

// MarkEnvironmentInactive marks the latest deployment of an environment inactive.
func (c *Client) MarkEnvironmentInactive(repo, env, description string) error {
	var ds []struct {
		ID int64 `json:"id"`
	}
	if err := c.do("GET", fmt.Sprintf("/repos/%s/%s/deployments?environment=%s&per_page=1", c.Org, repo, url.QueryEscape(env)), nil, &ds); err != nil {
		return err
	}
	if len(ds) == 0 {
		return nil
	}
	return c.DeploymentStatus(repo, ds[0].ID, "inactive", description, "", "")
}
