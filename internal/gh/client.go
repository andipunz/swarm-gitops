// Package gh is a small GitHub App client: installation auth, ETag-cached REST
// calls, GraphQL, and the few endpoints swarm-gitops needs. Stdlib only.
package gh

import (
	"bytes"
	"crypto"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
	"time"
)

// APIError is returned for non-2xx responses.
type APIError struct {
	Method, URL string
	StatusCode  int
	Body        string
}

func (e *APIError) Error() string {
	return fmt.Sprintf("github %s %s: %d %s", e.Method, e.URL, e.StatusCode, strings.TrimSpace(e.Body))
}

// IsNotFound reports whether err is a GitHub 404.
func IsNotFound(err error) bool {
	var ae *APIError
	return errors.As(err, &ae) && ae.StatusCode == http.StatusNotFound
}

type cached struct {
	etag string
	body []byte
}

// Client talks to the GitHub API as an App installation.
type Client struct {
	Org     string
	APIURL  string // e.g. https://api.github.com
	appID   int64
	key     *rsa.PrivateKey
	http    *http.Client
	mu      sync.Mutex
	instID  int64
	token   string
	expires time.Time
	etags   map[string]cached
}

// New creates a client. keyPEM is the App private key (PKCS#1 or PKCS#8).
func New(org, apiURL string, appID int64, keyPEM []byte) (*Client, error) {
	block, _ := pem.Decode(keyPEM)
	if block == nil {
		return nil, errors.New("github app key: no PEM block found")
	}
	var key *rsa.PrivateKey
	if k, err := x509.ParsePKCS1PrivateKey(block.Bytes); err == nil {
		key = k
	} else if k8, err8 := x509.ParsePKCS8PrivateKey(block.Bytes); err8 == nil {
		rk, ok := k8.(*rsa.PrivateKey)
		if !ok {
			return nil, errors.New("github app key: not an RSA key")
		}
		key = rk
	} else {
		return nil, fmt.Errorf("github app key: %v", err)
	}
	return &Client{
		Org:    org,
		APIURL: strings.TrimRight(apiURL, "/"),
		appID:  appID,
		key:    key,
		http:   &http.Client{Timeout: 30 * time.Second},
		etags:  map[string]cached{},
	}, nil
}

func (c *Client) appJWT() (string, error) {
	now := time.Now()
	hdr := base64.RawURLEncoding.EncodeToString([]byte(`{"alg":"RS256","typ":"JWT"}`))
	claims, _ := json.Marshal(map[string]any{
		"iat": now.Add(-60 * time.Second).Unix(),
		"exp": now.Add(9 * time.Minute).Unix(),
		"iss": fmt.Sprint(c.appID),
	})
	unsigned := hdr + "." + base64.RawURLEncoding.EncodeToString(claims)
	sum := sha256.Sum256([]byte(unsigned))
	sig, err := rsa.SignPKCS1v15(rand.Reader, c.key, crypto.SHA256, sum[:])
	if err != nil {
		return "", err
	}
	return unsigned + "." + base64.RawURLEncoding.EncodeToString(sig), nil
}

// installationToken returns a cached installation token, refreshing it early.
func (c *Client) installationToken() (string, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.token != "" && time.Until(c.expires) > 5*time.Minute {
		return c.token, nil
	}
	jwt, err := c.appJWT()
	if err != nil {
		return "", err
	}
	if c.instID == 0 {
		var inst struct {
			ID int64 `json:"id"`
		}
		if err := c.raw("GET", c.APIURL+"/orgs/"+c.Org+"/installation", "Bearer "+jwt, nil, &inst); err != nil {
			return "", fmt.Errorf("find app installation for org %s: %w", c.Org, err)
		}
		c.instID = inst.ID
	}
	var tok struct {
		Token     string    `json:"token"`
		ExpiresAt time.Time `json:"expires_at"`
	}
	url := fmt.Sprintf("%s/app/installations/%d/access_tokens", c.APIURL, c.instID)
	if err := c.raw("POST", url, "Bearer "+jwt, nil, &tok); err != nil {
		return "", fmt.Errorf("create installation token: %w", err)
	}
	c.token, c.expires = tok.Token, tok.ExpiresAt
	return c.token, nil
}

func (c *Client) raw(method, url, auth string, body any, out any) error {
	var rdr io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			return err
		}
		rdr = bytes.NewReader(b)
	}
	req, err := http.NewRequest(method, url, rdr)
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", auth)
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("X-GitHub-Api-Version", "2022-11-28")
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	data, _ := io.ReadAll(io.LimitReader(resp.Body, 50<<20))
	if resp.StatusCode/100 != 2 {
		return &APIError{Method: method, URL: url, StatusCode: resp.StatusCode, Body: truncate(string(data), 500)}
	}
	if out != nil && len(data) > 0 {
		return json.Unmarshal(data, out)
	}
	return nil
}

// do performs an authenticated REST call. GETs use conditional requests
// (ETag) so unchanged resources don't count against the rate limit.
func (c *Client) do(method, path string, body any, out any) error {
	tok, err := c.installationToken()
	if err != nil {
		return err
	}
	url := path
	if !strings.HasPrefix(path, "http") {
		url = c.APIURL + path
	}
	var rdr io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			return err
		}
		rdr = bytes.NewReader(b)
	}
	req, err := http.NewRequest(method, url, rdr)
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+tok)
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("X-GitHub-Api-Version", "2022-11-28")
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	c.mu.Lock()
	prev, havePrev := c.etags[url]
	c.mu.Unlock()
	if method == "GET" && havePrev {
		req.Header.Set("If-None-Match", prev.etag)
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	data, _ := io.ReadAll(io.LimitReader(resp.Body, 50<<20))
	switch {
	case resp.StatusCode == http.StatusNotModified && havePrev:
		data = prev.body
	case resp.StatusCode/100 != 2:
		return &APIError{Method: method, URL: url, StatusCode: resp.StatusCode, Body: truncate(string(data), 500)}
	case method == "GET" && resp.Header.Get("ETag") != "":
		c.mu.Lock()
		if len(c.etags) > 5000 {
			c.etags = map[string]cached{}
		}
		c.etags[url] = cached{etag: resp.Header.Get("ETag"), body: data}
		c.mu.Unlock()
	}
	if out != nil && len(data) > 0 {
		return json.Unmarshal(data, out)
	}
	return nil
}

// GraphQL runs a query. Any error in the response fails the whole call, so
// callers never act on partial data.
func (c *Client) GraphQL(query string, vars map[string]any, out any) error {
	var resp struct {
		Data   json.RawMessage `json:"data"`
		Errors []struct {
			Message string `json:"message"`
		} `json:"errors"`
	}
	if err := c.do("POST", "/graphql", map[string]any{"query": query, "variables": vars}, &resp); err != nil {
		return err
	}
	if len(resp.Errors) > 0 {
		msgs := make([]string, 0, len(resp.Errors))
		for _, e := range resp.Errors {
			msgs = append(msgs, e.Message)
		}
		return fmt.Errorf("graphql: %s", strings.Join(msgs, "; "))
	}
	return json.Unmarshal(resp.Data, out)
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
}
