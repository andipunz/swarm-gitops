// Package secretdriver asks a Docker secret driver plugin, over its socket,
// for the current version of the secrets it provides. This is not part of
// Docker's plugin protocol; see op-sa-secret-driver's README
// ("Change detection") for the request a plugin has to answer.
package secretdriver

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"strings"
	"time"
)

// ErrUnsupported means the plugin doesn't answer version requests.
var ErrUnsupported = errors.New("plugin does not support /SecretProvider.Versions")

// Secret is one Docker secret the plugin provides.
type Secret struct {
	Name   string
	Labels map[string]string `json:",omitempty"`
}

// Result is the plugin's answer: an opaque version per secret name, errors
// per secret name, and how long its cache may still serve an old value.
type Result struct {
	Versions map[string]string
	Errors   map[string]string
	CacheTTL time.Duration
}

// Versions posts the secrets to the plugin listening on socket.
func Versions(ctx context.Context, socket string, secrets []Secret) (*Result, error) {
	client := &http.Client{
		Timeout: 60 * time.Second,
		Transport: &http.Transport{DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
			var d net.Dialer
			return d.DialContext(ctx, "unix", socket)
		}},
	}
	body, err := json.Marshal(map[string]any{"Secrets": secrets})
	if err != nil {
		return nil, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, "http://plugin/SecretProvider.Versions", bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/vnd.docker.plugins.v1.2+json")
	resp, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	data, _ := io.ReadAll(io.LimitReader(resp.Body, 10<<20))
	var out struct {
		Versions        map[string]string
		Errors          map[string]string
		CacheTTLSeconds int
		Err             string
	}
	_ = json.Unmarshal(data, &out)
	switch {
	case resp.StatusCode == http.StatusNotFound || resp.StatusCode == http.StatusNotImplemented:
		return nil, ErrUnsupported
	case resp.StatusCode/100 != 2:
		msg := out.Err
		if msg == "" {
			msg = strings.TrimSpace(string(data))
		}
		return nil, fmt.Errorf("plugin: %d %s", resp.StatusCode, msg)
	}
	return &Result{Versions: out.Versions, Errors: out.Errors, CacheTTL: time.Duration(out.CacheTTLSeconds) * time.Second}, nil
}
