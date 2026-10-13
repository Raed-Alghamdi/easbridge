package main

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"easbridge/internal/install"
)

// MailFlow 3.9.0's two compose files, fetched unchanged from its v3.9.0 tag into compat/.mailflow-cache
// (git-ignored) and checked against these pins before they are used (as the compat harness does for its
// JavaScript). Nothing of them is copied into this repository.
var mailflowComposePins = map[string]string{
	"docker-compose.yml":      "4706327e956d59666a63f97c549dee70d8984123f926ab68435e55e68e053bc6",
	"docker-compose.ghcr.yml": "a558a219ad007a727703889321fb39ce835a79d9b5dff21e07c7c4212217df88",
}

func fetchMailflowCompose(t *testing.T, name string) []byte {
	t.Helper()
	cache := filepath.Join("..", "..", "compat", ".mailflow-cache", "v3.9.0", name)
	sum := func(b []byte) string { h := sha256.Sum256(b); return hex.EncodeToString(h[:]) }
	if b, err := os.ReadFile(cache); err == nil && sum(b) == mailflowComposePins[name] {
		return b
	}
	c := &http.Client{Timeout: 20 * time.Second}
	resp, err := c.Get("https://raw.githubusercontent.com/maathimself/mailflow/v3.9.0/" + name)
	if err != nil {
		t.Skipf("MailFlow's %s is not cached and cannot be fetched: %v", name, err)
	}
	defer resp.Body.Close()
	b, err := io.ReadAll(resp.Body)
	if err != nil || resp.StatusCode != 200 {
		t.Skipf("MailFlow's %s cannot be fetched: %v %d", name, err, resp.StatusCode)
	}
	if got := sum(b); got != mailflowComposePins[name] {
		t.Fatalf("%s from the v3.9.0 tag has SHA-256 %s, the pin is %s; refusing to use it", name, got, mailflowComposePins[name])
	}
	if err := os.MkdirAll(filepath.Dir(cache), 0o755); err == nil {
		os.WriteFile(cache, b, 0o644)
	}
	return b
}

// `docker compose config` needs the docker CLI with the compose plugin, not a running daemon.
func composeConfig(t *testing.T, dir string, files ...string) map[string]any {
	t.Helper()
	args := []string{"compose"}
	for _, f := range files {
		args = append(args, "-f", f)
	}
	args = append(args, "config", "--format", "json")
	cmd := exec.Command("docker", args...)
	cmd.Dir = dir
	cmd.Env = append(os.Environ(), "SESSION_SECRET=s", "DB_PASSWORD=d", "COMPOSE_PROJECT_NAME=easbridgetest")
	var out, errb bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &errb
	if err := cmd.Run(); err != nil {
		t.Fatalf("docker compose config: %v\n%s", err, errb.String())
	}
	var m map[string]any
	if err := json.Unmarshal(out.Bytes(), &m); err != nil {
		t.Fatalf("%v\n%s", err, out.String())
	}
	return m
}

func sub(m map[string]any, path ...string) any {
	var cur any = m
	for _, p := range path {
		mm, ok := cur.(map[string]any)
		if !ok {
			return nil
		}
		cur = mm[p]
	}
	return cur
}

// The generated override merges into MailFlow 3.9.0's own compose files: the backend keeps every
// environment variable and every depends_on entry it had and gains the CA, the read-only volume and the
// wait for a healthy bridge; the bridge has the hardening and publishes nothing.
func TestTheOverrideMergesWithMailFlows390ComposeFiles(t *testing.T) {
	if _, err := exec.LookPath("docker"); err != nil {
		t.Skip("docker is not installed")
	}
	if err := exec.Command("docker", "compose", "version").Run(); err != nil {
		t.Skip("docker compose is not available")
	}
	for _, name := range []string{"docker-compose.yml", "docker-compose.ghcr.yml"} {
		t.Run(name, func(t *testing.T) {
			base := fetchMailflowCompose(t, name)
			dir := t.TempDir()
			if err := os.WriteFile(filepath.Join(dir, name), base, 0o644); err != nil {
				t.Fatal(err)
			}
			out, _, err := initRun(t, dir, "", false, "--out", dir, "--eas-url", goodURL, "--users", "you@example.com,@example.org", "--image", "ghcr.io/example/easbridge:0.3.0")
			if err != nil {
				t.Fatalf("%v\n%s", err, out)
			}
			if strings.Contains(out, "WARNING") {
				t.Errorf("init warns about MailFlow's own file:\n%s", out)
			}
			plain := composeConfig(t, dir, name)
			merged := composeConfig(t, dir, name, install.OverrideName)

			// The backend keeps what it had.
			be, _ := sub(plain, "services", "backend", "environment").(map[string]any)
			bm, _ := sub(merged, "services", "backend", "environment").(map[string]any)
			if len(be) < 10 {
				t.Fatalf("the base environment of the backend was not read: %v", be)
			}
			for k, v := range be {
				if bm[k] != v {
					t.Errorf("backend environment %s: %v became %v", k, v, bm[k])
				}
			}
			if bm["NODE_EXTRA_CA_CERTS"] != "/easbridge-ca/ca.pem" {
				t.Errorf("NODE_EXTRA_CA_CERTS = %v", bm["NODE_EXTRA_CA_CERTS"])
			}
			dp, _ := sub(plain, "services", "backend", "depends_on").(map[string]any)
			dm, _ := sub(merged, "services", "backend", "depends_on").(map[string]any)
			if len(dp) < 2 {
				t.Fatalf("the base depends_on of the backend: %v", dp)
			}
			for k, v := range dp {
				if fmt.Sprint(dm[k]) != fmt.Sprint(v) {
					t.Errorf("backend depends_on %s: %v became %v", k, v, dm[k])
				}
			}
			if c, _ := sub(dm, "easbridge", "condition").(string); c != "service_healthy" {
				t.Errorf("the backend does not wait for a healthy bridge: %v", dm["easbridge"])
			}
			// Its volume is read only; its other settings did not change.
			var found bool
			for _, v := range sub(merged, "services", "backend", "volumes").([]any) {
				vm := v.(map[string]any)
				if vm["source"] == "easbridge-ca" {
					found = true
					if vm["target"] != "/easbridge-ca" || vm["read_only"] != true {
						t.Errorf("the CA volume in the backend: %v", vm)
					}
				}
			}
			if !found {
				t.Error("no CA volume in the backend")
			}
			for _, k := range []string{"container_name", "restart", "healthcheck", "networks"} {
				if fmt.Sprint(sub(plain, "services", "backend", k)) != fmt.Sprint(sub(merged, "services", "backend", k)) {
					t.Errorf("backend %s changed: %v -> %v", k, sub(plain, "services", "backend", k), sub(merged, "services", "backend", k))
				}
			}
			// Nothing else of MailFlow changed.
			for _, svc := range []string{"frontend", "postgres", "redis"} {
				if fmt.Sprint(sub(plain, "services", svc)) != fmt.Sprint(sub(merged, "services", svc)) {
					t.Errorf("the service %s changed", svc)
				}
			}
			// The bridge.
			eb, _ := sub(merged, "services", "easbridge").(map[string]any)
			if eb == nil {
				t.Fatal("no easbridge service")
			}
			if eb["image"] != "ghcr.io/example/easbridge:0.3.0" || eb["restart"] != "unless-stopped" || eb["read_only"] != true || eb["container_name"] != "easbridge" {
				t.Errorf("easbridge: %v", eb)
			}
			if fmt.Sprint(eb["cap_drop"]) != "[ALL]" || fmt.Sprint(eb["security_opt"]) != "[no-new-privileges:true]" {
				t.Errorf("hardening: %v %v", eb["cap_drop"], eb["security_opt"])
			}
			if fmt.Sprint(eb["mem_limit"]) != "1073741824" || fmt.Sprint(eb["pids_limit"]) != "256" {
				t.Errorf("limits: %v %v", eb["mem_limit"], eb["pids_limit"])
			}
			if hc, _ := eb["healthcheck"].(map[string]any); hc == nil || fmt.Sprint(hc["test"]) != "[CMD /easbridge healthcheck]" || fmt.Sprint(hc["interval"]) != "5s" {
				t.Errorf("the health check: %v", eb["healthcheck"])
			}
			if _, has := eb["ports"]; has {
				t.Errorf("the bridge publishes ports: %v", eb["ports"])
			}
			if _, ok := sub(merged, "services", "easbridge", "networks", "mailflow").(map[string]any); !ok {
				if _, ok2 := sub(merged, "services", "easbridge", "networks").(map[string]any)["mailflow"]; !ok2 {
					t.Errorf("easbridge is not on the mailflow network: %v", eb["networks"])
				}
			}
			env, _ := eb["environment"].(map[string]any)
			if env["EASBRIDGE_CA_EXPORT_DIR"] != "/ca" || env["EASBRIDGE_EAS_URL"] != goodURL || env["EASBRIDGE_ALLOWED_USERS"] != "you@example.com,@example.org" {
				t.Errorf("easbridge environment: %v", env)
			}
			if fmt.Sprint(sub(merged, "volumes", "easbridge-data")) == "<nil>" || fmt.Sprint(sub(merged, "volumes", "easbridge-ca")) == "<nil>" {
				t.Errorf("the named volumes are missing: %v", merged["volumes"])
			}
		})
	}
}
