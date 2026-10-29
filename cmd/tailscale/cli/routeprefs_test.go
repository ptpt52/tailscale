// Copyright (c) Tailscale Inc & contributors
// SPDX-License-Identifier: BSD-3-Clause

package cli

import (
	"flag"
	"strings"
	"testing"

	"tailscale.com/ipn"
	"tailscale.com/ipn/ipnstate"
)

func TestRoutePrefsFlags(t *testing.T) {
	for _, tc := range []struct {
		name, table string
		metric      int
		wantErr     bool
	}{
		{"main", "main", 4096, false},
		{"default", "52", 0, false},
		{"max", "main", 1<<31 - 1, false},
		{"negative", "main", -1, true},
		{"invalid-table", "100", 4096, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			args := upArgsFromOSArgs("linux")
			args.routeTable, args.routeMetric = tc.table, tc.metric
			p, err := prefsFromUpArgs(args, t.Logf, new(ipnstate.Status), "linux")
			if (err != nil) != tc.wantErr {
				t.Fatalf("prefsFromUpArgs error = %v", err)
			}
			if tc.wantErr {
				return
			}
			if p.RouteTableName() != tc.table || p.RouteMetric != tc.metric {
				t.Fatalf("unexpected prefs: %s", p.Pretty())
			}
			if got := prefValue("route-table", p, nil); got != tc.table {
				t.Fatalf("get route-table = %v", got)
			}
			if got := prefValue("route-metric", p, nil); got != tc.metric {
				t.Fatalf("get route-metric = %v", got)
			}
			flags := prefsToFlags(upCheckEnv{goos: "linux"}, p)
			if flags["route-table"] != tc.table || flags["route-metric"] != tc.metric {
				t.Fatalf("prefsToFlags lost route settings: %v", flags)
			}
		})
	}
	t.Run("up-keeps-custom-route-settings", func(t *testing.T) {
		var args upArgsT
		fs := newUpFlagSet("linux", &args, "up")
		if err := fs.Parse([]string{"--accept-routes"}); err != nil {
			t.Fatal(err)
		}
		p, err := prefsFromUpArgs(args, t.Logf, new(ipnstate.Status), "linux")
		if err != nil {
			t.Fatal(err)
		}
		cur := p.Clone()
		cur.RouteMetric, cur.RouteTable = 4096, "main"
		_, err = checkForAccidentalSettingReverts(p, cur, upCheckEnv{goos: "linux", flagSet: fs})
		if err == nil || !strings.Contains(err.Error(), "--route-metric=4096") || !strings.Contains(err.Error(), "--route-table=main") {
			t.Fatalf("up should require preserving custom route settings: %v", err)
		}
	})

	// tailscale set must only change explicitly supplied fields.
	var args setArgsT
	fs := newSetFlagSet("linux", &args)
	if err := fs.Parse([]string{"--route-metric=4096"}); err != nil {
		t.Fatal(err)
	}
	var mp ipn.MaskedPrefs
	if err := setRoutePrefs(&mp.Prefs, args.routeMetric, args.routeTable); err != nil {
		t.Fatal(err)
	}
	fs.Visit(func(f *flag.Flag) { updateMaskedPrefsFromUpOrSetFlag(&mp, f.Name) })
	p := &ipn.Prefs{RouteTable: "main", RouteAll: true}
	p.ApplyEdits(&mp)
	if p.RouteMetric != 4096 || p.RouteTable != "main" || !p.RouteAll {
		t.Fatalf("set unexpectedly changed other preferences: %s", p.Pretty())
	}
}
