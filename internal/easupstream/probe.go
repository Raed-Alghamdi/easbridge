package easupstream

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"sort"
	"strings"
	"time"

	"github.com/hstern/go-activesync/wbxml"

	"easbridge/internal/store"
	"easbridge/internal/upstream"
)

// ProbeConfig drives Probe.
type ProbeConfig struct {
	Config
	User     string
	Password string
	DeviceID string
	// PingSeconds is the length of the final Ping (0 skips it).
	PingSeconds int
	// Out receives the report.
	Out io.Writer
	// Trace prints, under every request, the decoded request and response with all
	// content redacted (`easbridge probe --trace`; see renderXML).
	Trace bool
	// SkipContacts omits the contacts step and the directory step (`--skip-contacts`).
	SkipContacts bool
	// SearchSpacing is the time between the two GAL searches of the directory step; the
	// default is the 20 seconds the service keeps between any two searches.
	SearchSpacing time.Duration
}

// Probe is the first look at a real Exchange server (DESIGN section 12): OPTIONS
// and Provision, FolderSync, a 5-item header-only Sync of the Inbox, one full
// fetch (size only) and a Ping. It prints status codes, counts, sizes and
// timings, and nothing that identifies mail: no addresses, subjects, folder
// names or bodies. Nothing is stored; the sync keys live in memory only.
func Probe(ctx context.Context, pc ProbeConfig) error {
	out := pc.Out
	if out == nil {
		out = io.Discard
	}
	pc.Config.WindowSize = 5
	pc.Config.setDefaults()

	// Every HTTP request of the probe is timed (command, status and where the time
	// went; nothing else), so that a slow step can be told apart: more requests
	// than needed, a slow connection, or a slow answer.
	rec := &requestTrace{messages: pc.Trace, reg: wbxml.DefaultRegistry().TolerateUnknownTags()}
	hc := pc.Config.HTTPClient
	if hc == nil {
		var err error
		if hc, err = newHTTPClient(pc.Config); err != nil {
			return err
		}
	}
	traced := *hc
	base := traced.Transport
	if base == nil {
		base = http.DefaultTransport
	}
	traced.Transport = &tracingTransport{base: base, rec: rec}
	pc.Config.HTTPClient = &traced

	fac, err := NewFactory(pc.Config)
	if err != nil {
		return err
	}
	up, err := fac.New(upstream.Config{User: pc.User, Password: pc.Password, DeviceID: pc.DeviceID})
	if err != nil {
		return err
	}
	mb := up.(*Mailbox)
	defer mb.Close()

	step := func(name string) func(detail string, err error) error {
		start := time.Now()
		mark := rec.mark()
		return func(detail string, err error) error {
			el := time.Since(start).Round(time.Millisecond)
			defer func() {
				for _, r := range rec.since(mark) {
					fmt.Fprintf(out, "             http: %s\n", r)
					for _, d := range r.Detail {
						fmt.Fprintf(out, "               %s\n", d)
					}
				}
			}()
			if err != nil {
				fmt.Fprintf(out, "%-12s FAILED  %s  (%v)\n", name, describe(err), el)
				return err
			}
			fmt.Fprintf(out, "%-12s ok      %s  (%v)\n", name, detail, el)
			return nil
		}
	}

	fmt.Fprintf(out, "device id    %s (shared with the service)\n", pc.DeviceID)
	c := pc.Config
	fmt.Fprintf(out, "device       type=%s model=%q friendly_name=%q os=%q os_language=%q user_agent=%q "+
		"(sent in the initial Provision request on protocol 14.1 and later; no IMEI, phone number or mobile operator)\n",
		c.DeviceType, c.DeviceModel, c.DeviceFriendlyName, c.DeviceOS, c.DeviceOSLanguage, c.UserAgent)
	done := step("options+prov")
	err = mb.Open(ctx)
	if e := done(fmt.Sprintf("protocol=%s policy_key_set=%v", mb.Version(), mb.PolicyKey() != ""), err); e != nil {
		return e
	}

	done = step("foldersync")
	fd, err := mb.FolderSync(ctx, "0")
	var inbox string
	if err == nil {
		counts := map[int]int{}
		for _, f := range fd.Added {
			counts[f.Type]++
			if f.Type == store.TypeInbox && inbox == "" {
				inbox = f.ServerID
			}
		}
		types := make([]int, 0, len(counts))
		for t := range counts {
			types = append(types, t)
		}
		sort.Ints(types)
		detail := fmt.Sprintf("folders=%d by_type=", len(fd.Added))
		for i, t := range types {
			if i > 0 {
				detail += ","
			}
			detail += fmt.Sprintf("%d:%d", t, counts[t])
		}
		err = done(detail, nil)
		if err == nil && inbox == "" {
			err = errors.New("the hierarchy has no Inbox (type 2)")
			done("", err)
		}
	} else {
		err = done("", err)
	}
	if err != nil {
		return err
	}

	done = step("sync boot")
	d, err := mb.Sync(ctx, inbox, "0", upstream.SyncOptions{HeadersOnly: true})
	if e := done("key issued", err); e != nil {
		return e
	}
	done = step("sync 5 hdrs")
	d2, err := mb.Sync(ctx, inbox, d.NewKey, upstream.SyncOptions{HeadersOnly: true})
	var first string
	if err == nil {
		unread := 0
		sizes := ""
		for i, it := range d2.Added {
			if i == 0 {
				first = it.ServerID
			}
			if !it.Read {
				unread++
			}
			sizes += fmt.Sprintf(" %d", it.EstimatedSize)
		}
		err = done(fmt.Sprintf("items=%d unread=%d more=%v est_sizes_bytes=[%s ]", len(d2.Added), unread, d2.MoreAvailable, sizes), nil)
	} else {
		err = done("", err)
	}
	if err != nil {
		return err
	}

	if first != "" {
		done = step("fetch 1 mime")
		mime, err := mb.FetchMIME(ctx, inbox, first)
		est := 0
		if len(d2.Added) > 0 {
			est = d2.Added[0].EstimatedSize
		}
		if e := done(fmt.Sprintf("bytes=%d announced=%d", len(mime), est), err); e != nil {
			return e
		}
	} else {
		fmt.Fprintf(out, "%-12s skipped (the Inbox is empty or the filter hides everything)\n", "fetch 1 mime")
	}

	if pc.PingSeconds > 0 {
		done = step("ping")
		pctx, cancel := context.WithTimeout(ctx, time.Duration(pc.PingSeconds)*time.Second+90*time.Second)
		res, err := mb.Ping(pctx, time.Duration(pc.PingSeconds)*time.Second, []string{inbox})
		cancel()
		detail := ""
		if res != nil {
			detail = fmt.Sprintf("status=%d changed_folders=%d server_heartbeat_s=%d heartbeat_s=%d",
				res.Status, len(res.Changed), int(res.Heartbeat/time.Second), pc.PingSeconds)
		}
		if e := done(detail, err); e != nil {
			return e
		}
	}
	var optional error
	if !pc.SkipContacts {
		optional = probeContacts(ctx, pc, mb, fd, step, out)
	}
	fmt.Fprintln(out, "probe finished")
	return optional
}

// probeContacts is the contacts step and the directory step of the probe. They come
// after the mail steps; a failure is reported and does not stop the other one, but the
// probe then ends with an error. Only statuses, counts and the names of fields are
// printed, never a value.
func probeContacts(ctx context.Context, pc ProbeConfig, mb *Mailbox, fd *upstream.FolderDelta, step func(string) func(string, error) error, out io.Writer) error {
	var failed error
	note := func(err error) {
		if err != nil && failed == nil {
			failed = err
		}
	}
	// Contacts: the folders by type, then one Sync of the default folder.
	var def string
	n9, n14 := 0, 0
	for _, f := range fd.Added {
		switch f.Type {
		case store.TypeContacts:
			n9++
			if def == "" {
				def = f.ServerID
			}
		case store.TypeUserContacts:
			n14++
		}
	}
	if def == "" {
		fmt.Fprintf(out, "%-12s skipped (no default Contacts folder, type 9, in the hierarchy; type 14 folders: %d)\n", "contacts", n14)
	} else {
		fmt.Fprintf(out, "%-12s ok      contact folders: type9=%d type14=%d\n", "contacts", n9, n14)
		done := step("ctc boot")
		d, err := mb.SyncContacts(ctx, def, "0")
		if e := done("key issued", err); e != nil {
			note(e)
		} else {
			done = step("ctc sync")
			d2, err := mb.SyncContacts(ctx, def, d.NewKey)
			detail := ""
			if err == nil {
				detail = fmt.Sprintf("items=%d changed=%d deleted=%d more=%v fields=[%s]", len(d2.Added), len(d2.Changed), len(d2.Deleted), d2.MoreAvailable, strings.Join(d2.Fields, ","))
			}
			note(done(detail, err))
		}
	}

	// Directory: the account's own full address, then its local part. Two searches, as far
	// apart as the service keeps any two.
	spacing := pc.SearchSpacing
	if spacing <= 0 {
		spacing = 20 * time.Second
	}
	local, _, _ := strings.Cut(pc.User, "@")
	type galStep struct {
		name, query string
		prefix      bool
	}
	steps := []galStep{{"gal address", pc.User, false}}
	if local != "" && local != pc.User {
		steps = append(steps, galStep{"gal local", local, false})
	}
	// The short queries of the opt-in full sweep: does the directory answer a search for the first 1
	// and the first 2 characters of the account's own local part, and is the account's entry in the answer?
	lr := []rune(local)
	for n := 1; n <= 2; n++ {
		if n < len(lr) {
			steps = append(steps, galStep{fmt.Sprintf("gal prefix %d", n), string(lr[:n]), true})
		}
	}
	for i, st := range steps {
		if i > 0 {
			select {
			case <-time.After(spacing):
			case <-ctx.Done():
				note(ctx.Err())
				return failed
			}
		}
		done := step(st.name)
		res, err := mb.SearchDirectory(ctx, st.query)
		detail := ""
		if err == nil {
			exact := false
			for _, e := range res.Entries {
				if strings.EqualFold(strings.TrimSpace(e.EmailAddress), pc.User) {
					exact = true
				}
			}
			if st.prefix {
				detail = fmt.Sprintf("total=%d entries=%d own_entry=%v", res.Total, len(res.Entries), exact)
			} else {
				detail = fmt.Sprintf("total=%d entries=%d exact_match=%v fields=[%s]", res.Total, len(res.Entries), exact, strings.Join(res.Fields, ","))
			}
		}
		note(done(detail, err))
	}
	return failed
}

// describe renders an error without anything identifying mail.
func describe(err error) string {
	var se *upstream.StatusError
	switch {
	case errors.Is(err, upstream.ErrAuthFailed):
		return "authentication failed (HTTP 401): wrong password, or the account is locked"
	case errors.Is(err, upstream.ErrAccountOnlyWipe):
		return "the server ordered an account-only remote wipe"
	case errors.Is(err, upstream.ErrRemoteWipe):
		return "the server ordered a remote wipe"
	case errors.Is(err, upstream.ErrUnavailable):
		return "server unavailable or network error: " + err.Error()
	case errors.Is(err, upstream.ErrNoMIME):
		// The first real Fetch failed this way although Exchange had sent the message (as an inline string,
		// which the library did not read as MIME): the trace tells which form the data came in.
		return "Fetch: Exchange answered, but the answer holds no MIME body the bridge can read " +
			"(easbridge probe --trace shows the body element: its Type, and whether the Data came as text or as binary)"
	case errors.As(err, &se):
		// Op, the code, whether the same request could ever work, and what the
		// status means (statuses.go) when it is known.
		if se.Reason != "" {
			return fmt.Sprintf("%s status %d permanent=%v: %s", se.Op, se.Code, se.Perm, se.Reason)
		}
		return fmt.Sprintf("%s status %d permanent=%v", se.Op, se.Code, se.Perm)
	}
	return err.Error()
}
