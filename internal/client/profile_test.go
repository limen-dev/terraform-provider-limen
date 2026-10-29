package client_test

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/limen-dev/terraform-provider-limen/internal/client"
	"github.com/limen-dev/terraform-provider-limen/internal/fakelimen"
)

func writeFile(path, content string) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	return os.WriteFile(path, []byte(content), 0o600)
}

func TestProfileDirMatchesTheCLI(t *testing.T) {
	// The same mapping as limen-cli/src/profile.rs: anything but
	// [A-Za-z0-9.-] becomes '_'.
	got := client.ProfileDir("/h", "limen.example.com:443")
	if got != filepath.Join("/h", "profiles", "limen.example.com_443") {
		t.Fatal(got)
	}
	if got := client.ProfileDir("/h", "[::1]:3443"); got != filepath.Join("/h", "profiles", "___1__3443") {
		t.Fatal(got)
	}
}

func TestReusesTheCLILogin(t *testing.T) {
	srv, err := fakelimen.Start(fakelimen.Options{})
	if err != nil {
		t.Fatal(err)
	}
	defer srv.Close()
	srv.AddAdmin("alice")
	home := t.TempDir()
	auth := &client.ProfileAuth{Home: home, Proxy: srv.Address}
	if _, err := auth.Login(context.Background(), nil, nil); err == nil || !strings.Contains(err.Error(), "limen login") {
		t.Fatalf("without a login: %v", err)
	}
	cert, key, err := srv.IssueCertificate("alice", time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	dir := client.ProfileDir(home, srv.Address)
	for name, content := range map[string]string{
		"cert.pem":     cert,
		"key.pem":      key,
		"proxy-ca.pem": srv.CAPEM,
		"profile.json": `{"proxy":"x","username":"alice","roles":["limen:admin"],"login_id":"l","expires_at_unix":1}`,
	} {
		if err := writeFile(filepath.Join(dir, name), content); err != nil {
			t.Fatal(err)
		}
	}
	if err := writeFile(filepath.Join(home, "current"), srv.Address+"\n"); err != nil {
		t.Fatal(err)
	}
	if client.CurrentProxy(home) != srv.Address {
		t.Fatal("current proxy")
	}
	c, err := client.New(context.Background(), client.Config{
		Address: srv.Address, CACertPEM: client.PinnedCA(home, srv.Address), Auth: auth,
	})
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	if c.Principal() != "alice" {
		t.Fatal(c.Principal())
	}
}
