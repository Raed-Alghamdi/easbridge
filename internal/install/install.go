// Package install makes the files `easbridge init` writes next to MailFlow's compose file, and checks
// what it can of that compose file as text (there is no YAML library in the project, and a check
// that cannot tell is a warning, never an error).
package install

import (
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
)

// File names written by init.
const (
	OverrideName = "docker-compose.easbridge.yml"
	EnvName      = "easbridge.env"
)

// MailFlow's compose files, in the order init looks for them.
var ComposeNames = []string{"docker-compose.yml", "docker-compose.ghcr.yml"}

// Params are the values the generated files hold.
type Params struct {
	EASURL         string
	Users          []string
	Image          string
	BackendService string
	Network        string
}

// EnvFile is easbridge.env: the two values of the install. No secret: the password is typed into
// MailFlow, and into `easbridge probe` when it is asked for.
func EnvFile(p Params) string {
	return "# EASBridge settings, written by `easbridge init`. Nothing secret belongs here: the mailbox\n" +
		"# password is typed into MailFlow (and into `easbridge probe`, which asks for it).\n" +
		"EASBRIDGE_EAS_URL=" + p.EASURL + "\n" +
		"EASBRIDGE_ALLOWED_USERS=" + strings.Join(p.Users, ",") + "\n"
}

// Override is docker-compose.easbridge.yml: the easbridge service, and the three lines that make
// MailFlow's backend trust the certificate the bridge makes and wait for the bridge.
func Override(p Params) string {
	r := strings.NewReplacer("@IMAGE@", p.Image, "@BACKEND@", p.BackendService, "@NETWORK@", p.Network)
	return r.Replace(overrideTemplate)
}

const overrideTemplate = `# Written by ` + "`easbridge init`" + `. Use it together with MailFlow's compose file:
#   docker compose -f <MailFlow's compose file> -f docker-compose.easbridge.yml up -d
# It adds the easbridge service (no published ports, nothing writable but its two volumes) and makes
# MailFlow's backend trust the certificate EASBridge makes for itself and start after it is healthy.
services:
  easbridge:
    image: @IMAGE@
    container_name: easbridge
    restart: unless-stopped
    read_only: true
    cap_drop:
      - ALL
    security_opt:
      - no-new-privileges:true
    mem_limit: 1g
    pids_limit: 256
    ulimits:
      core: 0
    logging:
      driver: json-file
      options:
        max-size: "50m"
        max-file: "3"
    # The image's own check (the binary asks its health endpoint; the image has no shell), run every 5 s
    # so that MailFlow's backend, which waits for it, starts a few seconds after the bridge and not 30.
    healthcheck:
      test: ["CMD", "/easbridge", "healthcheck"]
      interval: 5s
      timeout: 5s
      retries: 6
      start_period: 15s
    env_file:
      - easbridge.env
    environment:
      EASBRIDGE_CA_EXPORT_DIR: /ca
    volumes:
      - easbridge-data:/data
      - easbridge-ca:/ca
    networks:
      - @NETWORK@

  @BACKEND@:
    environment:
      NODE_EXTRA_CA_CERTS: /easbridge-ca/ca.pem
    volumes:
      - easbridge-ca:/easbridge-ca:ro
    depends_on:
      easbridge:
        condition: service_healthy

volumes:
  easbridge-data:
  easbridge-ca:
`

// CheckValues returns the problems that would break the env file or the YAML: values with
// characters those formats treat specially.
func CheckValues(p Params) error {
	for _, v := range []struct{ name, val string }{{"the ActiveSync URL", p.EASURL}, {"the logins", strings.Join(p.Users, ",")},
		{"--image", p.Image}, {"--backend-service", p.BackendService}, {"--network", p.Network}} {
		if v.val == "" {
			return fmt.Errorf("%s is empty", v.name)
		}
		if strings.ContainsAny(v.val, " \t\r\n\"'#$\\`") {
			return fmt.Errorf("%s (%q) holds a space, a quote, #, $ or a backslash, which an env file or a compose file would read differently", v.name, v.val)
		}
	}
	return nil
}

var serviceName = regexp.MustCompile(`^[a-zA-Z0-9][a-zA-Z0-9._-]*$`)

// CheckNames validates the service and network names.
func CheckNames(p Params) error {
	if !serviceName.MatchString(p.BackendService) {
		return fmt.Errorf("--backend-service %q is not a service name", p.BackendService)
	}
	if !serviceName.MatchString(p.Network) {
		return fmt.Errorf("--network %q is not a network name", p.Network)
	}
	return nil
}

// FindCompose looks for MailFlow's compose file in dir. It returns the name found ("" when there is
// none) and, when both exist, a note saying which one it took.
func FindCompose(dir string) (name, note string) {
	var found []string
	for _, n := range ComposeNames {
		if st, err := os.Stat(filepath.Join(dir, n)); err == nil && st.Mode().IsRegular() {
			found = append(found, n)
		}
	}
	switch len(found) {
	case 0:
		return "", ""
	case 1:
		return found[0], ""
	}
	return found[0], fmt.Sprintf("both %s and %s are here; the commands below use %s (use the other one in them if that is the file you run)", found[0], found[1], found[0])
}

// CheckCompose reads a compose file as text and returns warnings: the backend service is not there,
// the network is not there. It never fails the install; an unusual layout just produces a warning.
func CheckCompose(text, backend, network string) []string {
	var w []string
	if !hasKeyUnder(text, "services", backend) {
		w = append(w, fmt.Sprintf("no service called %q in the compose file; if MailFlow's backend has another name, run init again with --backend-service NAME", backend))
	}
	if !hasKeyUnder(text, "networks", network) {
		w = append(w, fmt.Sprintf("no network called %q in the compose file; if MailFlow's services use another network, run init again with --network NAME", network))
	}
	return w
}

// hasKeyUnder reports whether the top-level block `top:` has a direct child `key:`. Comments and
// blank lines do not end the block; the next line at column 0 does.
func hasKeyUnder(text, top, key string) bool {
	in := false
	indent := -1
	for _, line := range strings.Split(text, "\n") {
		line = strings.TrimRight(line, "\r")
		trim := strings.TrimSpace(line)
		if trim == "" || strings.HasPrefix(trim, "#") {
			continue
		}
		col := len(line) - len(strings.TrimLeft(line, " "))
		name, rest, _ := strings.Cut(trim, ":")
		if col == 0 {
			rest = strings.TrimSpace(rest)
			in = strings.Trim(name, `"'`) == top && (rest == "" || strings.HasPrefix(rest, "#"))
			indent = -1
			continue
		}
		if !in {
			continue
		}
		if indent < 0 {
			indent = col
		}
		if col == indent && strings.Trim(strings.TrimSpace(name), `"'`) == key {
			return true
		}
	}
	return false
}

// Compose is the command prefix for the next steps.
func Compose(composeFile string) string {
	if composeFile == "" {
		composeFile = ComposeNames[0]
	}
	return "docker compose -f " + composeFile + " -f " + OverrideName
}

// ProbeLogin is the login the probe example shows: the first plain address, or a placeholder when
// the list holds only domain entries.
func ProbeLogin(users []string) string {
	for _, u := range users {
		if !strings.HasPrefix(u, "@") {
			return u
		}
	}
	return "<your mailbox login>"
}

// NextSteps are printed by init and are the steps of INSTALL.md. The labels are those of MailFlow
// 3.9.0's interface (frontend/src/locales/en.json and AdminPanel.jsx at the v3.9.0 tag).
func NextSteps(p Params, composeFile string) string {
	c := Compose(composeFile)
	login := ProbeLogin(p.Users)
	file := composeFile
	if file == "" {
		file = ComposeNames[0]
	}
	return fmt.Sprintf(`Next steps

 To type less, tell Compose once per shell which files to use; then plain "docker compose up -d", "docker compose logs easbridge"
 and so on work, without the -f flags below:

      export COMPOSE_FILE=%[3]s:%[4]s

 1. Optional, first contact with Exchange. Run it before step 2: the probe and the service share
    /data. It asks for the mailbox password without echoing it, and tells you at once if Exchange
    refuses the device, the login or the URL:

      %[1]s run --rm easbridge probe --user %[2]s

 2. Start EASBridge. MailFlow's backend starts after it is healthy and trusts its certificate:

      %[1]s up -d

 3. In MailFlow, as an administrator: Settings > Security & Integrations > Security > Mail Server
    Connection Policy > turn on "Allow private / local hosts" (MailFlow refuses a mail server at
    a private address until you do, and EASBridge is one).

 4. In MailFlow: Settings > Account & Mail > Accounts > "Add account" > "Custom", then:
      Display name              any name you like (MailFlow requires it)
      Email address             your mailbox address
      Username (IMAP login)     %[2]s
      Password / App password   the mailbox password
      IMAP Settings   IMAP Host: easbridge   Port: 993
      SMTP Settings   SMTP Host: easbridge   Port: 465
    MailFlow uses TLS on ports 993 and 465 and verifies the certificate with the CA of step 2: leave
    "Skip TLS certificate verification" off (it is not even shown unless the administrator allows it).

 5. Contacts: Settings > Security & Integrations > Integrations > "Apps" > "CardDAV Contacts":
      Server URL      https://easbridge:8443/
      Username        %[2]s
      App password    the same mailbox password
    then "Connect".

More: INSTALL.md (upgrading, uninstalling, troubleshooting).
`, c, login, file, OverrideName)
}
