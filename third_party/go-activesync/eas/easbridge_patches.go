// Copyright (C) 2026 Henry Stern
// SPDX-License-Identifier: MIT
//
// This file is an easbridge addition (patches 3, 7 and 10, see PATCHES.md). It is
// not part of upstream go-activesync.

package eas

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"strings"

	"github.com/hstern/go-activesync/wbxml"
)

// ApplyOptions tunes ApplyEmailChangesWith.
type ApplyOptions struct {
	// HardDelete sends DeletesAsMoves=0, so Delete commands remove items for
	// good instead of moving them to Deleted Items. Use it only when deleting
	// from Deleted Items itself. easbridge patch 3.
	HardDelete bool
}

// ApplyResult is the outcome of ApplyEmailChangesWith.
type ApplyResult struct {
	// Results holds the per-command statuses from the <Responses> element.
	// Commands that succeeded without anything to report may be absent.
	Results []EmailChangeResult
	// SyncKey is the key the server returned (already handed to
	// State.SetSyncKey). Empty if the server answered with an empty body.
	SyncKey string
	// ServerChanges holds server-side Add/Change/Delete commands that came
	// back in the same response, or nil. upstream's ApplyEmailChanges stores
	// the new SyncKey and throws such commands away, which loses them for
	// good because the server never resends changes covered by an accepted
	// key. easbridge patch 7.
	ServerChanges *EmailSyncResult
}

// ErrUnsupportedClient is returned by ApplyEmailChangesWith when the Client
// was not built by NewClient (for example a mock).
var ErrUnsupportedClient = errors.New("eas: client does not support ApplyEmailChangesWith")

// ApplyEmailChangesWith issues a Sync with client-originated commands for one
// folder. Differences from Client.ApplyEmailChanges:
//
//   - it sends GetChanges=0 (the upstream code intends this but omits it), and
//     still returns any server commands the response carries (ServerChanges);
//   - DeletesAsMoves is selectable (ApplyOptions.HardDelete);
//   - it never bootstraps or resets the folder key by itself. The caller must
//     already hold a non-"0" key; on InvalidSyncKey (status 3) the StatusError
//     is returned untouched so the caller can run its own reconciliation.
func ApplyEmailChangesWith(ctx context.Context, cl Client, folderID string, changes []EmailChange, opts ApplyOptions) (*ApplyResult, error) {
	c, ok := cl.(*httpClient)
	if !ok {
		return nil, ErrUnsupportedClient
	}
	if folderID == "" {
		return nil, errors.New("eas: ApplyEmailChangesWith: folderID is required")
	}
	key, err := c.cfg.State.SyncKey(ctx, folderID)
	if err != nil {
		return nil, fmt.Errorf("eas: ApplyEmailChangesWith: read sync key: %w", err)
	}
	if key == "" || key == "0" {
		return nil, errors.New("eas: ApplyEmailChangesWith: folder has no sync key yet; run SyncEmail first")
	}
	cmds := buildChangeCommands(changes)
	if len(cmds.Children) == 0 {
		return nil, errors.New("eas: ApplyEmailChangesWith: no effective changes")
	}
	deletesAsMoves := "1"
	if opts.HardDelete {
		deletesAsMoves = "0"
	}
	collection := wbxml.E(wbxml.PageAirSync, "Collection",
		wbxml.E(wbxml.PageAirSync, "SyncKey", wbxml.Text(key)),
		wbxml.E(wbxml.PageAirSync, "CollectionId", wbxml.Text(folderID)),
		wbxml.E(wbxml.PageAirSync, "DeletesAsMoves", wbxml.Text(deletesAsMoves)),
		wbxml.E(wbxml.PageAirSync, "GetChanges", wbxml.Text("0")),
		cmds,
	)
	doc := &wbxml.Document{
		Root: wbxml.E(wbxml.PageAirSync, "Sync",
			wbxml.E(wbxml.PageAirSync, "Collections", collection),
		),
	}
	resp, err := c.post(ctx, "Sync", doc)
	if err != nil {
		return nil, err
	}
	out := &ApplyResult{}
	if resp == nil || resp.Root == nil {
		return out, nil
	}
	if st := topStatus(resp.Root); st != 0 && st != StatusOK {
		return nil, &StatusError{Command: "Sync", Code: st}
	}
	respCollection := resp.Root.Find("Collection")
	if respCollection == nil {
		return nil, errors.New("eas: ApplyEmailChangesWith: response missing <Collection>")
	}
	if cs := findShallow(respCollection, "Status", 1); cs != nil {
		if code := atoi(cs.TextContent()); code != 0 && code != StatusOK {
			return nil, &StatusError{Command: "Sync", Code: code}
		}
	}
	if k := findShallow(respCollection, "SyncKey", 1); k != nil {
		out.SyncKey = k.TextContent()
		if out.SyncKey != "" {
			if err := c.cfg.State.SetSyncKey(ctx, folderID, out.SyncKey); err != nil {
				return nil, fmt.Errorf("eas: ApplyEmailChangesWith: persist key: %w", err)
			}
		}
	}
	if responses := findShallow(respCollection, "Responses", 1); responses != nil {
		for _, ch := range responses.Children {
			el, ok := ch.(*wbxml.Element)
			if !ok {
				continue
			}
			r := EmailChangeResult{}
			if id := el.Find("ServerId"); id != nil {
				r.ServerID = id.TextContent()
			}
			if st := el.Find("Status"); st != nil {
				r.Status = atoi(st.TextContent())
			}
			out.Results = append(out.Results, r)
		}
	}
	server := &EmailSyncResult{SyncKey: out.SyncKey}
	if findShallow(respCollection, "MoreAvailable", 1) != nil {
		server.MoreAvailable = true
	}
	parseSyncCommands(respCollection, server)
	if len(server.Added)+len(server.Changed)+len(server.Deleted) > 0 || server.MoreAvailable {
		out.ServerChanges = server
	}
	return out, nil
}

// DeviceInformationInProvision reports whether the device information travels
// inside the initial Provision request on protocol version v: 14.1, 16.0 and
// 16.1 (MS-ASPROV 2.2.2.53, "MUST send ... when sending an initial Provision
// command request to the server but not on subsequent requests"). On 14.0, 12.x
// and 2.5 it must not be sent there; 14.0 and 12.x can carry it in a Settings
// command instead (MS-ASCMD 2.2.3.167). A version that does not parse gives
// false. easbridge patch 10.
func DeviceInformationInProvision(v string) bool {
	maj, rest, ok := strings.Cut(strings.TrimSpace(v), ".")
	if !ok {
		return false
	}
	major, err1 := strconv.Atoi(maj)
	minor, err2 := strconv.Atoi(rest)
	if err1 != nil || err2 != nil || major < 0 || minor < 0 {
		return false
	}
	return major > 14 || (major == 14 && minor >= 1)
}
