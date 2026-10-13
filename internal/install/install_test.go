package install

import (
	"flag"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

var update = flag.Bool("update", false, "rewrite the golden files")

func example() Params {
	return Params{EASURL: "https://mail.example.com/Microsoft-Server-ActiveSync", Users: []string{"you@example.com", "@example.org"},
		Image: "ghcr.io/example/easbridge:0.3.0", BackendService: "backend", Network: "mailflow"}
}

func golden(t *testing.T, name, got string) {
	t.Helper()
	p := filepath.Join("testdata", name)
	if *update {
		if err := os.WriteFile(p, []byte(got), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	want, err := os.ReadFile(p)
	if err != nil {
		t.Fatal(err)
	}
	if string(want) != got {
		t.Errorf("%s differs from what init writes (run go test ./internal/install -update to rewrite it after a deliberate change):\n--- got ---\n%s", name, got)
	}
}

func TestGeneratedFilesMatchTheGoldenFiles(t *testing.T) {
	p := example()
	golden(t, "override.golden.yml", Override(p))
	golden(t, "easbridge.golden.env", EnvFile(p))
	golden(t, "nextsteps.golden.txt", NextSteps(p, "docker-compose.yml"))
	p2 := Params{EASURL: "https://mail.example.com/Microsoft-Server-ActiveSync", Users: []string{"@example.org"}, Image: "ghcr.io/example/easbridge:0.3.0", BackendService: "api", Network: "internal"}
	golden(t, "override-renamed.golden.yml", Override(p2))
	golden(t, "nextsteps-domain-ghcr.golden.txt", NextSteps(p2, "docker-compose.ghcr.yml"))
}

// What the override must say, independently of the golden text: the hardening, no ports, the CA read
// only in the backend, the backend waiting for a healthy bridge.
func TestTheOverrideHasTheHardeningAndTheTrustWiring(t *testing.T) {
	y := Override(example())
	for _, want := range []string{
		"restart: unless-stopped", "read_only: true", "cap_drop:\n      - ALL", "no-new-privileges:true", "mem_limit: 1g", "pids_limit: 256",
		"ulimits:\n      core: 0", "driver: json-file", "env_file:\n      - easbridge.env", `test: ["CMD", "/easbridge", "healthcheck"]`, "interval: 5s", "EASBRIDGE_CA_EXPORT_DIR: /ca",
		"easbridge-data:/data", "easbridge-ca:/ca\n", "container_name: easbridge",
		"NODE_EXTRA_CA_CERTS: /easbridge-ca/ca.pem", "easbridge-ca:/easbridge-ca:ro", "condition: service_healthy",
		"\nvolumes:\n  easbridge-data:\n  easbridge-ca:\n", "networks:\n      - mailflow",
	} {
		if !strings.Contains(y, want) {
			t.Errorf("the override lacks %q", want)
		}
	}
	for _, bad := range []string{"ports:", "network_mode", "privileged", "- /var/run/docker.sock", "password", "PASSWORD"} {
		if strings.Contains(y, bad) {
			t.Errorf("the override has %q", bad)
		}
	}
	var keys []string
	for _, l := range strings.Split(EnvFile(example()), "\n") {
		if l != "" && !strings.HasPrefix(l, "#") {
			keys = append(keys, strings.SplitN(l, "=", 2)[0])
		}
	}
	if strings.Join(keys, ",") != "EASBRIDGE_EAS_URL,EASBRIDGE_ALLOWED_USERS" {
		t.Errorf("the env file holds %v: only the URL and the logins, nothing secret", keys)
	}
}

func TestCheckValues(t *testing.T) {
	ok := example()
	if err := CheckValues(ok); err != nil {
		t.Fatal(err)
	}
	for name, mut := range map[string]func(*Params){
		"a space in the url":  func(p *Params) { p.EASURL = "https://a.test/x y" },
		"a hash in the url":   func(p *Params) { p.EASURL = "https://a.test/x#y" },
		"a quote in the user": func(p *Params) { p.Users = []string{`a"b@x.test`} },
		"a dollar":            func(p *Params) { p.Image = "x/$y" },
		"empty image":         func(p *Params) { p.Image = "" },
		"empty url":           func(p *Params) { p.EASURL = "" },
	} {
		p := example()
		mut(&p)
		if CheckValues(p) == nil {
			t.Errorf("%s accepted", name)
		}
	}
	for _, bad := range []string{"", "a b", "-x", "a/b", "a:b"} {
		p := example()
		p.BackendService = bad
		if CheckNames(p) == nil {
			t.Errorf("service %q accepted", bad)
		}
		p = example()
		p.Network = bad
		if CheckNames(p) == nil {
			t.Errorf("network %q accepted", bad)
		}
	}
}

const mailflowLike = `services:

  # comment
  frontend:
    image: x
    networks:
      - mailflow

  backend:
    image: y
    networks:
      - mailflow

volumes:
  postgres_data:

networks:
  mailflow:
    driver: bridge
`

func TestCheckComposeAsText(t *testing.T) {
	if w := CheckCompose(mailflowLike, "backend", "mailflow"); len(w) != 0 {
		t.Errorf("warnings for a good file: %v", w)
	}
	w := CheckCompose(mailflowLike, "api", "mailflow")
	if len(w) != 1 || !strings.Contains(w[0], `"api"`) || !strings.Contains(w[0], "--backend-service NAME") {
		t.Errorf("backend: %v", w)
	}
	w = CheckCompose(mailflowLike, "backend", "internal")
	if len(w) != 1 || !strings.Contains(w[0], `"internal"`) || !strings.Contains(w[0], "--network NAME") {
		t.Errorf("network: %v", w)
	}
	if w := CheckCompose(mailflowLike, "x", "y"); len(w) != 2 {
		t.Errorf("both: %v", w)
	}
	// A name that is only inside another block is not the service or the network.
	trick := "services:\n  web:\n    environment:\n      backend: 1\n    networks:\n      - mailflow\n"
	if w := CheckCompose(trick, "backend", "mailflow"); len(w) != 2 {
		t.Errorf("nested names counted: %v", w)
	}
	// Quoted keys, CRLF, a comment after the block name.
	odd := "services: # all of them\r\n  \"backend\":\r\n    image: y\r\nnetworks:\r\n  mailflow: {}\r\n"
	if w := CheckCompose(odd, "backend", "mailflow"); len(w) != 0 {
		t.Errorf("odd formatting: %v", w)
	}
	if hasKeyUnder("", "services", "backend") {
		t.Error("empty text")
	}
}

func TestFindCompose(t *testing.T) {
	dir := t.TempDir()
	if n, note := FindCompose(dir); n != "" || note != "" {
		t.Errorf("empty folder: %q %q", n, note)
	}
	os.WriteFile(filepath.Join(dir, "docker-compose.ghcr.yml"), []byte("services:\n"), 0o644)
	if n, note := FindCompose(dir); n != "docker-compose.ghcr.yml" || note != "" {
		t.Errorf("ghcr only: %q %q", n, note)
	}
	os.WriteFile(filepath.Join(dir, "docker-compose.yml"), []byte("services:\n"), 0o644)
	if n, note := FindCompose(dir); n != "docker-compose.yml" || !strings.Contains(note, "both docker-compose.yml and docker-compose.ghcr.yml") {
		t.Errorf("both: %q %q", n, note)
	}
	// A directory of that name is no compose file.
	d2 := t.TempDir()
	os.Mkdir(filepath.Join(d2, "docker-compose.yml"), 0o755)
	if n, _ := FindCompose(d2); n != "" {
		t.Errorf("a directory counted: %q", n)
	}
}

func TestProbeLogin(t *testing.T) {
	if ProbeLogin([]string{"@example.org", "a@example.org"}) != "a@example.org" || ProbeLogin([]string{"@example.org"}) != "<your mailbox login>" {
		t.Error("probe login")
	}
}
