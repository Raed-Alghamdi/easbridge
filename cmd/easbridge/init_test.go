package main

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"easbridge/internal/install"
	"easbridge/internal/version"
)

const goodURL = "https://mail.example.com/Microsoft-Server-ActiveSync"

const mailflowCompose = "services:\n  backend:\n    image: y\n    networks:\n      - mailflow\nnetworks:\n  mailflow:\n    driver: bridge\n"

func initRun(t *testing.T, dir string, stdin string, tty bool, args ...string) (string, string, error) {
	t.Helper()
	var out, errOut bytes.Buffer
	a := append([]string{"--out", dir}, args...)
	err := runInit(a, strings.NewReader(stdin), &out, &errOut, tty)
	return out.String(), errOut.String(), err
}

func read(t *testing.T, p string) string {
	t.Helper()
	b, err := os.ReadFile(p)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

func TestInitWithFlagsWritesTheTwoFilesAndPrintsTheSteps(t *testing.T) {
	dir := t.TempDir()
	os.WriteFile(filepath.Join(dir, "docker-compose.yml"), []byte(mailflowCompose), 0o644)
	out, _, err := initRun(t, dir, "", false, "--eas-url", goodURL, "--users", "You@Example.com, @Example.org", "--image", "ghcr.io/example/easbridge:0.3.0")
	if err != nil {
		t.Fatal(err)
	}
	wantOverride := install.Override(install.Params{EASURL: goodURL, Users: []string{"you@example.com", "@example.org"}, Image: "ghcr.io/example/easbridge:0.3.0", BackendService: "backend", Network: "mailflow"})
	if got := read(t, filepath.Join(dir, install.OverrideName)); got != wantOverride {
		t.Errorf("override:\n%s", got)
	}
	env := read(t, filepath.Join(dir, install.EnvName))
	if !strings.Contains(env, "EASBRIDGE_EAS_URL="+goodURL+"\n") || !strings.Contains(env, "EASBRIDGE_ALLOWED_USERS=you@example.com,@example.org\n") {
		t.Errorf("env:\n%s", env)
	}
	for _, f := range []string{install.OverrideName, install.EnvName} {
		if st, _ := os.Stat(filepath.Join(dir, f)); st.Mode().Perm() != 0o644 {
			t.Errorf("%s mode %v", f, st.Mode())
		}
	}
	for _, want := range []string{
		"docker compose -f docker-compose.yml -f docker-compose.easbridge.yml run --rm easbridge probe --user you@example.com",
		"docker compose -f docker-compose.yml -f docker-compose.easbridge.yml up -d",
		"export COMPOSE_FILE=docker-compose.yml:docker-compose.easbridge.yml", "Display name              any name you like",
		`"Allow private / local hosts"`, `"Add account" > "Custom"`, "IMAP Host: easbridge   Port: 993", "SMTP Host: easbridge   Port: 465",
		"https://easbridge:8443/", `"CardDAV Contacts"`,
	} {
		if !strings.Contains(out, want) {
			t.Errorf("the steps lack %q:\n%s", want, out)
		}
	}
	if strings.Contains(out, "WARNING") {
		t.Errorf("warnings for a good setup:\n%s", out)
	}
	if strings.Contains(out+env, "assword=") {
		t.Error("a password-looking value")
	}
}

func TestInitDefaultsToTheImageOfThisBuild(t *testing.T) {
	dir := t.TempDir()
	if _, _, err := initRun(t, dir, "", false, "--eas-url", goodURL, "--users", "a@example.com"); err != nil {
		t.Fatal(err)
	}
	if got := read(t, filepath.Join(dir, install.OverrideName)); !strings.Contains(got, "image: "+version.ImageRef()+"\n") {
		t.Errorf("image: %s", got)
	}
	if !strings.HasPrefix(version.ImageRef(), "ghcr.io/") || !strings.HasSuffix(version.ImageRef(), ":"+version.Version) {
		t.Errorf("image ref %q", version.ImageRef())
	}
}

func TestInitAsksForWhatIsMissingAtATerminal(t *testing.T) {
	dir := t.TempDir()
	// A bad URL (http), then a good one; a bad user list, then a good one.
	in := "http://mail.example.com/x\n" + goodURL + "\n@bad domain\n\nyou@example.com\n"
	out, _, err := initRun(t, dir, in, true)
	if err != nil {
		t.Fatalf("%v\n%s", err, out)
	}
	if !strings.Contains(out, "ActiveSync URL") || !strings.Contains(out, "must be an https:// URL") || !strings.Contains(out, "is not a domain entry") {
		t.Errorf("prompts and complaints:\n%s", out)
	}
	if !strings.Contains(read(t, filepath.Join(dir, install.EnvName)), "EASBRIDGE_ALLOWED_USERS=you@example.com\n") {
		t.Error("the answers were not used")
	}
	// Only the missing value is asked.
	dir2 := t.TempDir()
	out, _, err = initRun(t, dir2, "you@example.com\n", true, "--eas-url", goodURL)
	if err != nil || strings.Contains(out, "ActiveSync URL") || !strings.Contains(out, "Logins that may use EASBRIDGE") && !strings.Contains(out, "Logins that may use EASBridge") {
		t.Errorf("%v\n%s", err, out)
	}
	// Three bad answers, and an input that ends, are errors.
	if _, _, err := initRun(t, t.TempDir(), "x\ny\nz\n", true); err == nil {
		t.Error("three bad URLs were accepted")
	}
	if _, _, err := initRun(t, t.TempDir(), "", true); err == nil {
		t.Error("no input was accepted")
	}
}

func TestInitWithoutATerminalNamesTheMissingFlags(t *testing.T) {
	for _, tc := range []struct {
		args []string
		want string
	}{
		{nil, "--eas-url and --users needed"},
		{[]string{"--users", "a@example.com"}, "--eas-url needed"},
		{[]string{"--eas-url", goodURL}, "--users needed"},
		{[]string{"--eas-url", " ", "--users", "a@example.com"}, "--eas-url needed"},
	} {
		dir := t.TempDir()
		_, _, err := initRun(t, dir, "ignored\n", false, tc.args...)
		if err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Errorf("%q: %v", tc.args, err)
		}
		if ents, _ := os.ReadDir(dir); len(ents) != 0 {
			t.Errorf("%q: files were written", tc.args)
		}
	}
}

func TestInitValidatesWhatItWrites(t *testing.T) {
	for name, tc := range map[string]struct {
		args []string
		want string
	}{
		"http":               {[]string{"--eas-url", "http://mail.example.com/Microsoft-Server-ActiveSync", "--users", "a@example.com"}, "https://"},
		"no host":            {[]string{"--eas-url", "https:///x", "--users", "a@example.com"}, "no host"},
		"not a url":          {[]string{"--eas-url", "mail.example.com", "--users", "a@example.com"}, "https://"},
		"bad domain entry":   {[]string{"--eas-url", goodURL, "--users", "a@example.com,@bad_domain"}, "not a domain entry"},
		"only separators":    {[]string{"--eas-url", goodURL, "--users", " , ,"}, "--users"},
		"a space in the url": {[]string{"--eas-url", "https://a.test/x y", "--users", "a@example.com"}, "space"},
		"a quote in a login": {[]string{"--eas-url", goodURL, "--users", `a"b@example.com`}, "quote"},
		"bad service name":   {[]string{"--eas-url", goodURL, "--users", "a@example.com", "--backend-service", "my backend"}, "--backend-service"},
		"bad network name":   {[]string{"--eas-url", goodURL, "--users", "a@example.com", "--network", "a:b"}, "--network"},
		"an image with a $":  {[]string{"--eas-url", goodURL, "--users", "a@example.com", "--image", "x/$y"}, "--image"},
		"an argument":        {[]string{"--eas-url", goodURL, "--users", "a@example.com", "stray"}, "flags only"},
		"an unknown flag":    {[]string{"--nope"}, "nope"},
	} {
		t.Run(name, func(t *testing.T) {
			dir := t.TempDir()
			_, _, err := initRun(t, dir, "", false, tc.args...)
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("want %q, got %v", tc.want, err)
			}
			if ents, _ := os.ReadDir(dir); len(ents) != 0 {
				t.Errorf("files were written for an invalid input: %v", ents)
			}
		})
	}
	// A folder that is not there.
	_, _, err := initRun(t, filepath.Join(t.TempDir(), "nope"), "", false, "--eas-url", goodURL, "--users", "a@example.com")
	if err == nil || !strings.Contains(err.Error(), `-v "$PWD:/out"`) {
		t.Errorf("missing folder: %v", err)
	}
}

func TestInitWarnsAboutWhatItCannotConfirm(t *testing.T) {
	good := []string{"--eas-url", goodURL, "--users", "a@example.com"}
	t.Run("no compose file", func(t *testing.T) {
		out, _, err := initRun(t, t.TempDir(), "", false, good...)
		if err != nil || !strings.Contains(out, "WARNING: no docker-compose.yml or docker-compose.ghcr.yml") || !strings.Contains(out, "-f docker-compose.yml -f docker-compose.easbridge.yml up -d") {
			t.Errorf("%v\n%s", err, out)
		}
	})
	t.Run("the ghcr file is used when it is the only one", func(t *testing.T) {
		dir := t.TempDir()
		os.WriteFile(filepath.Join(dir, "docker-compose.ghcr.yml"), []byte(mailflowCompose), 0o644)
		out, _, err := initRun(t, dir, "", false, good...)
		if err != nil || strings.Contains(out, "WARNING") || !strings.Contains(out, "-f docker-compose.ghcr.yml -f docker-compose.easbridge.yml up -d") ||
			!strings.Contains(out, "export COMPOSE_FILE=docker-compose.ghcr.yml:docker-compose.easbridge.yml") {
			t.Errorf("%v\n%s", err, out)
		}
	})
	t.Run("both files", func(t *testing.T) {
		dir := t.TempDir()
		os.WriteFile(filepath.Join(dir, "docker-compose.ghcr.yml"), []byte(mailflowCompose), 0o644)
		os.WriteFile(filepath.Join(dir, "docker-compose.yml"), []byte(mailflowCompose), 0o644)
		out, _, _ := initRun(t, dir, "", false, good...)
		if !strings.Contains(out, "both docker-compose.yml and docker-compose.ghcr.yml") {
			t.Errorf("%s", out)
		}
	})
	t.Run("the backend and the network are not there", func(t *testing.T) {
		dir := t.TempDir()
		os.WriteFile(filepath.Join(dir, "docker-compose.yml"), []byte("services:\n  web:\n    image: x\n"), 0o644)
		out, _, err := initRun(t, dir, "", false, good...)
		if err != nil || !strings.Contains(out, `no service called "backend"`) || !strings.Contains(out, "--backend-service NAME") ||
			!strings.Contains(out, `no network called "mailflow"`) || !strings.Contains(out, "--network NAME") {
			t.Errorf("%v\n%s", err, out)
		}
		// The flags fix it, and the names go into the file.
		os.WriteFile(filepath.Join(dir, "docker-compose.yml"), []byte("services:\n  web:\n    networks: [n]\nnetworks:\n  n: {}\n"), 0o644)
		out, _, err = initRun(t, dir, "", false, append(good, "--backend-service", "web", "--network", "n", "--force")...)
		if err != nil || strings.Contains(out, "WARNING") {
			t.Errorf("%v\n%s", err, out)
		}
		if y := read(t, filepath.Join(dir, install.OverrideName)); !strings.Contains(y, "\n  web:\n") || !strings.Contains(y, "networks:\n      - n\n") {
			t.Errorf("names not used:\n%s", y)
		}
	})
	t.Run("a path that is not the usual one, a login that is not an address", func(t *testing.T) {
		out, _, err := initRun(t, t.TempDir(), "", false, "--eas-url", "https://mail.example.com/owa", "--users", "jsmith")
		if err != nil || !strings.Contains(out, `the path of the URL is "/owa"`) || !strings.Contains(out, `the login "jsmith" is not an address`) {
			t.Errorf("%v\n%s", err, out)
		}
		out, _, _ = initRun(t, t.TempDir(), "", false, "--eas-url", "https://mail.example.com/microsoft-server-activesync/", "--users", `DOM\jsmith`)
		if strings.Contains(out, "WARNING: the path") || strings.Contains(out, "is not an address") {
			t.Errorf("a case difference, a trailing slash and DOMAIN\\user are fine:\n%s", out)
		}
	})
}

func TestInitNeverOverwritesWithoutForce(t *testing.T) {
	args := []string{"--eas-url", goodURL, "--users", "a@example.com"}
	for _, existing := range []string{install.OverrideName, install.EnvName} {
		dir := t.TempDir()
		os.WriteFile(filepath.Join(dir, existing), []byte("mine\n"), 0o644)
		_, _, err := initRun(t, dir, "", false, args...)
		if err == nil || !strings.Contains(err.Error(), existing) || !strings.Contains(err.Error(), "--force") {
			t.Fatalf("%s: %v", existing, err)
		}
		if read(t, filepath.Join(dir, existing)) != "mine\n" {
			t.Errorf("%s was overwritten", existing)
		}
		other := install.EnvName
		if existing == install.EnvName {
			other = install.OverrideName
		}
		if _, err := os.Stat(filepath.Join(dir, other)); err == nil {
			t.Errorf("%s was written although %s stood in the way", other, existing)
		}
		if _, _, err := initRun(t, dir, "", false, append(args, "--force")...); err != nil {
			t.Fatal(err)
		}
		if read(t, filepath.Join(dir, existing)) == "mine\n" {
			t.Errorf("%s was not replaced with --force", existing)
		}
	}
	// A symlink in the way is not followed silently either.
	dir := t.TempDir()
	os.Symlink(filepath.Join(dir, "elsewhere"), filepath.Join(dir, install.EnvName))
	if _, _, err := initRun(t, dir, "", false, args...); err == nil {
		t.Error("a dangling symlink at the file's name was written through")
	}
}

func TestInitHelpIsNotAnError(t *testing.T) {
	_, errOut, err := initRun(t, t.TempDir(), "", false, "-h")
	if err != nil || !strings.Contains(errOut, `--user "$(id -u):$(id -g)"`) || !strings.Contains(errOut, "-eas-url") {
		t.Errorf("%v\n%s", err, errOut)
	}
}
