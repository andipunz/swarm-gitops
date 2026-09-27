package config

import (
	"os"
	"path/filepath"
	"testing"
)

func clearAuthEnv() {
	for _, k := range []string{"GITHUB_APP_ID", "GITHUB_APP_KEY_FILE", "GITHUB_TOKEN", "GITHUB_TOKEN_FILE"} {
		os.Unsetenv(k)
	}
}

func TestLoadAuth(t *testing.T) {
	t.Run("github app", func(t *testing.T) {
		clearAuthEnv()
		t.Setenv("GITHUB_APP_ID", "42")
		t.Setenv("GITHUB_APP_KEY_FILE", "/key.pem")
		c := &Config{}
		if err := c.loadAuth(); err != nil {
			t.Fatal(err)
		}
		if c.AppID != 42 || c.AppKeyFile != "/key.pem" || c.Token != "" {
			t.Fatalf("%+v", c)
		}
	})

	t.Run("personal access token", func(t *testing.T) {
		clearAuthEnv()
		t.Setenv("GITHUB_TOKEN", "ghp_x")
		c := &Config{}
		if err := c.loadAuth(); err != nil {
			t.Fatal(err)
		}
		if c.Token != "ghp_x" || c.AppID != 0 || c.AppKeyFile != "" {
			t.Fatalf("%+v", c)
		}
	})

	t.Run("token from file, trimmed", func(t *testing.T) {
		clearAuthEnv()
		f := filepath.Join(t.TempDir(), "token")
		if err := os.WriteFile(f, []byte("  ghp_y\n"), 0o600); err != nil {
			t.Fatal(err)
		}
		t.Setenv("GITHUB_TOKEN_FILE", f)
		c := &Config{}
		if err := c.loadAuth(); err != nil {
			t.Fatal(err)
		}
		if c.Token != "ghp_y" {
			t.Fatalf("token = %q", c.Token)
		}
	})

	t.Run("nothing configured", func(t *testing.T) {
		clearAuthEnv()
		if err := (&Config{}).loadAuth(); err == nil {
			t.Fatal("expected an error")
		}
	})

	t.Run("app and token both set is rejected", func(t *testing.T) {
		clearAuthEnv()
		t.Setenv("GITHUB_APP_ID", "42")
		t.Setenv("GITHUB_APP_KEY_FILE", "/key.pem")
		t.Setenv("GITHUB_TOKEN", "ghp_x")
		if err := (&Config{}).loadAuth(); err == nil {
			t.Fatal("expected an error")
		}
	})

	t.Run("app id without key file is rejected", func(t *testing.T) {
		clearAuthEnv()
		t.Setenv("GITHUB_APP_ID", "42")
		if err := (&Config{}).loadAuth(); err == nil {
			t.Fatal("expected an error")
		}
	})
}

func TestEnvSet(t *testing.T) {
	const key = "SWARM_GITOPS_TEST_ENVSET"

	t.Run("unset falls back to the default", func(t *testing.T) {
		os.Unsetenv(key)
		if v := envSet(key, "def"); v != "def" {
			t.Fatalf("got %q, want %q", v, "def")
		}
	})

	t.Run("explicitly empty is honored, not defaulted", func(t *testing.T) {
		t.Setenv(key, "")
		if v := envSet(key, "def"); v != "" {
			t.Fatalf("got %q, want empty", v)
		}
	})

	t.Run("non-empty value is used as-is", func(t *testing.T) {
		t.Setenv(key, "value")
		if v := envSet(key, "def"); v != "value" {
			t.Fatalf("got %q, want %q", v, "value")
		}
	})
}

// Regression test: ENV_PROPERTY="" and REQUIRE_PROTECTED="" used to
// silently fall back to their non-empty defaults (env(), not envSet()),
// making "no custom property gate" / "no protected-branch requirement"
// unreachable via configuration despite being documented, valid values.
func TestFromEnvHonorsExplicitEmpty(t *testing.T) {
	clearAuthEnv()
	t.Setenv("GITHUB_TOKEN", "ghp_x")
	t.Setenv("GITHUB_ORG", "example")
	t.Setenv("PREP_IMAGE", "ghcr.io/example/swarm-gitops:latest")
	t.Setenv("ENV_PROPERTY", "")
	t.Setenv("REQUIRE_PROTECTED", "")

	c, err := FromEnv()
	if err != nil {
		t.Fatal(err)
	}
	if c.EnvProperty != "" {
		t.Errorf("EnvProperty = %q, want empty", c.EnvProperty)
	}
	if len(c.RequireProtected) != 0 {
		t.Errorf("RequireProtected = %v, want empty", c.RequireProtected)
	}
}
