package easupstream

import (
	"strings"
	"testing"
	"time"

	"github.com/hstern/go-activesync/eas"

	"easbridge/internal/config"
)

// go-activesync names some statuses one code off (see PATCHES.md, "Not patched").
// The bridge must never show those names, so every code the library mislabels has
// a text of its own here, and none of the texts uses a name that belongs to
// another number.
func TestEveryStatusTheLibraryMislabelsHasATextOfItsOwn(t *testing.T) {
	for _, code := range []int{141, 142, 143, 144, 145, 177} {
		f, ok := lookupStatus("Provision", code)
		if !ok || f.msg == "" {
			t.Errorf("status %d has no text of its own (the library calls it %q)", code, (&eas.StatusError{Code: code}).Error())
		}
	}
	// The names of MS-ASCMD 2.2.2 that the library attaches to the wrong number.
	wrong := map[int][]string{
		141: {"DeviceNotProvisioned"},
		142: {"PolicyRefresh"},
		143: {"InvalidPolicyKey"},
		144: {"ExternallyManagedDevicesNotAllowed"},
		145: {"NoRecurrenceInCalendar"},
		177: {"ItemNotFoundInCollection"},
	}
	for code, names := range wrong {
		f, _ := lookupStatus("Provision", code)
		for _, n := range names {
			if strings.Contains(f.msg, n) {
				t.Errorf("status %d: %q uses %q, which is the name of another status", code, f.msg, n)
			}
		}
	}
}

func TestStatusTextsAreOneLineAndPlain(t *testing.T) {
	check := func(kind string, code int, f statusFacts) {
		t.Helper()
		switch {
		case f.msg == "":
			t.Errorf("%s %d: empty text", kind, code)
		case strings.ContainsAny(f.msg, "\r\n\t"):
			t.Errorf("%s %d: %q is not one line", kind, code, f.msg)
		case strings.HasSuffix(f.msg, "."):
			t.Errorf("%s %d: %q ends with a period", kind, code, f.msg)
		case strings.ContainsRune(f.msg, rune(0x2014)) || strings.ContainsRune(f.msg, rune(0x2013)):
			t.Errorf("%s %d: %q has a dash", kind, code, f.msg)
		case len(f.msg) > 200:
			t.Errorf("%s %d: %d characters", kind, code, len(f.msg))
		case !f.global:
			t.Errorf("%s %d: a status of the device or the account must be marked global", kind, code)
		}
	}
	for c, f := range commonStatus {
		if c < 100 {
			t.Errorf("common status %d is below 100", c)
		}
		check("common status", c, f)
	}
	for c, f := range provisionStatus {
		check("Provision status", c, f)
	}
	for c, f := range policyStatus {
		check("Policy status", c, f)
	}
	for c, f := range syncStatus {
		f.global = true // the check is about the text; whether Sync statuses are global is told below
		check("Sync status", c, f)
	}
}

// The classification the brief asks for: what an administrator must fix is
// permanent, what a new Provision cures is not.
func TestStatusClassification(t *testing.T) {
	for _, tc := range []struct {
		cmd  string
		code int
		perm bool
	}{
		{"Provision", 139, true}, {"Provision", 141, true}, {"Provision", 145, true}, {"Provision", 165, true},
		{"Sync", 129, true}, {"Sync", 130, true}, {"Sync", 131, true}, {"Sync", 177, true},
		{"Sync", 140, false}, {"Sync", 142, false}, {"Sync", 143, false}, {"Sync", 144, false},
		{"Sync", 110, false}, {"Sync", 111, false}, {"Sync", 114, false}, {"Sync", 115, false},
		{"Provision", 2, true}, {"Provision", 3, false},
		{"Provision/Policy", 3, true}, {"Provision/Policy", 4, true}, {"Provision/Policy", 5, false},
	} {
		f, ok := lookupStatus(tc.cmd, tc.code)
		if !ok || f.perm != tc.perm {
			t.Errorf("%s %d: known=%v permanent=%v, want permanent=%v", tc.cmd, tc.code, ok, f.perm, tc.perm)
		}
	}
	// Small codes keep meaning what their command says: Provision 3 is a server
	// error, Sync 3 is an invalid sync key (handled elsewhere), Policy 3 is a policy type.
	if _, ok := lookupStatus("Sync", 3); ok {
		t.Errorf("Sync status 3 must not be described as a Provision status")
	}
	// Sync 4 is the protocol error of the second real contact: permanent, and about
	// the request, not about the device.
	if f, ok := lookupStatus("Sync", 4); !ok || !f.perm || f.global || !strings.Contains(f.msg, "protocol error") || !strings.Contains(f.msg, "--trace") {
		t.Errorf("Sync 4: %+v %v", f, ok)
	}
	if f, ok := lookupStatus("Sync", 5); !ok || f.perm {
		t.Errorf("Sync 5 is temporary: %+v %v", f, ok)
	}
	if f, ok := lookupStatus("Sync", 13); !ok || f.perm {
		t.Errorf("Sync 13 is temporary: %+v %v", f, ok)
	}
	if a, _ := lookupStatus("Provision", 3); a.perm {
		t.Errorf("Provision 3 is temporary")
	}
	if b, _ := lookupStatus("Provision/Policy", 3); !b.perm {
		t.Errorf("Policy 3 is permanent")
	}
	// The fallback for codes not in the tables.
	if permanentStatus(5) || permanentStatus(9) || permanentStatus(13) || !permanentStatus(8) || !permanentStatus(6) {
		t.Errorf("permanentStatus changed")
	}
}

// The adapter's own defaults (used when a caller names nothing, as the tests do)
// are the device settings of the config package, so that no code path sends
// another description of the bridge than the documented one.
func TestDeviceDefaultsAgreeWithTheConfigPackage(t *testing.T) {
	var c Config
	c.setDefaults()
	d := config.Defaults()
	if c.DeviceType != d.DeviceType || c.DeviceModel != d.DeviceModel || c.DeviceFriendlyName != d.DeviceFriendlyName ||
		c.DeviceOS != d.DeviceOS || c.DeviceOSLanguage != d.DeviceOSLanguage {
		t.Errorf("adapter defaults %q %q %q %q %q, config defaults %q %q %q %q %q",
			c.DeviceType, c.DeviceModel, c.DeviceFriendlyName, c.DeviceOS, c.DeviceOSLanguage,
			d.DeviceType, d.DeviceModel, d.DeviceFriendlyName, d.DeviceOS, d.DeviceOSLanguage)
	}
}

func TestTracedRequestsAreDescribedWithoutAnythingThatIdentifies(t *testing.T) {
	ms := time.Millisecond
	for _, tc := range []struct {
		in   tracedRequest
		want string
	}{
		{tracedRequest{Method: "OPTIONS", Status: 200, Connect: 210 * ms, Wait: 690 * ms, Total: 905 * ms},
			"OPTIONS -> HTTP 200 in 905ms (new connection 210ms, answer after 690ms)"},
		{tracedRequest{Method: "POST", Cmd: "Provision", Status: 200, Reused: true, Wait: 5123 * ms, Total: 5130 * ms},
			"POST Provision -> HTTP 200 in 5.13s (connection reused, answer after 5.12s)"},
		{tracedRequest{Method: "POST", Cmd: "Sync", Total: 30 * time.Second}, "POST Sync -> no answer after 30s"},
		{tracedRequest{Method: "POST", Cmd: "Ping", Status: 200, Reused: true, Wait: 61234 * ms, Total: 61240 * ms},
			"POST Ping -> HTTP 200 in 1m1.2s (connection reused, answer after 1m1.2s)"},
	} {
		if got := tc.in.String(); got != tc.want {
			t.Errorf("got  %q\nwant %q", got, tc.want)
		}
	}
}
