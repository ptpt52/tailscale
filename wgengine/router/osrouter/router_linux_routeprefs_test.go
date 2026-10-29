// Copyright (c) Tailscale Inc & contributors
// SPDX-License-Identifier: BSD-3-Clause

package osrouter

import (
	"errors"
	"net/netip"
	"runtime"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/tailscale/netlink"
	"github.com/vishvananda/netns"
	"go4.org/netipx"
	"golang.org/x/sys/unix"
	"tailscale.com/envknob"
	"tailscale.com/tstest"
)

func TestLinuxRoutePreferences(t *testing.T) {
	r, os := newTestLinuxRouter(t)
	if err := r.Set(nil); err != nil {
		t.Fatal(err)
	}
	if len(os.rules) != 0 {
		t.Fatalf("Up installed rules before receiving preferences: %v", os.rules)
	}
	lan := "192.168.16.0/24 dev br-lan table main metric 0"
	os.routes = []string{lan}
	cfg := &Config{
		RouteTable:  "main",
		RouteMetric: 4096,
		Routes:      mustCIDRs("192.168.16.0/24", "100.64.0.2/32", "fd7a:115c:a1e0::2/128"),
		LocalRoutes: mustCIDRs("192.168.16.0/24", "fe80::/64"),
	}
	check := func(table string, metric int) {
		t.Helper()
		cfg.RouteTable, cfg.RouteMetric = table, metric
		if err := r.Set(cfg); err != nil {
			t.Fatal(err)
		}
		if !slices.Contains(os.routes, lan) {
			t.Fatal("LAN route was removed")
		}
		wantTable := "52"
		wantRoutes := 1 + len(cfg.Routes) + len(cfg.LocalRoutes)
		if table == "main" {
			wantTable = "main"
			wantRoutes -= len(cfg.LocalRoutes) // no throw routes in main
			if len(os.rules) != 0 {
				t.Fatalf("main table has policy rules: %v", os.rules)
			}
		} else if len(os.rules) != 8 {
			t.Fatalf("table 52 missing policy rules: %v", os.rules)
		}
		if len(os.routes) != wantRoutes {
			t.Fatalf("route count = %d, want %d: %v", len(os.routes), wantRoutes, os.routes)
		}
		for _, route := range os.routes {
			if route == lan {
				continue
			}
			if !strings.HasSuffix(route, "table "+wantTable+" metric "+strconv.Itoa(metric)) {
				t.Fatalf("stale table or metric: %s", route)
			}
		}
	}
	check("main", 4096)
	check("main", 8192)
	check("", 4096)
	// Queue a restore before switching modes. It must not restore rules after
	// the main table has been selected.
	r.onIPRuleDeleted(52, 5270)
	check("main", 4096)
	if err := tstest.WaitFor(3*time.Second, func() error {
		if r.ruleRestorePending.Load() {
			return errors.New("rule restoration timer is still pending")
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if len(os.rules) != 0 {
		t.Fatalf("timer restored policy rules in main mode: %v", os.rules)
	}
	if err := r.Set(nil); err != nil {
		t.Fatal(err)
	}
	if len(os.rules) != 0 || !slices.Equal(os.routes, []string{lan}) {
		t.Fatalf("shutdown left routes/rules or removed LAN: %s", os)
	}
	check("", 4096)
	check("main", 4096)
	if err := r.Close(); err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(os.routes, []string{lan}) {
		t.Fatalf("Close changed LAN or left routes: %v", os.routes)
	}
}

func TestLinuxRouteServiceOverrides(t *testing.T) {
	for _, tc := range []struct {
		name, table, metric, profileTable, wantTable string
		wantMetric                                   int
	}{
		{"main", "main", "4096", "", "main", 4096},
		{"policy", "52", "0", "main", "", 0},
		{"metric-only", "", "4096", "", "", 4096},
		{"table-only", "main", "", "", "main", 7},
		{"unset", "", "", "main", "main", 7},
	} {
		t.Run(tc.name, func(t *testing.T) {
			envknob.SetenvForTest(t, "TS_ROUTE_TABLE", tc.table)
			envknob.SetenvForTest(t, "TS_ROUTE_METRIC", tc.metric)
			r, os := newTestLinuxRouter(t)
			if err := r.Set(nil); err != nil {
				t.Fatal(err)
			}
			if len(os.rules) != 0 {
				t.Fatalf("startup installed rules before preferences: %v", os.rules)
			}
			cfg := &Config{Routes: mustCIDRs("100.64.0.2/32"), RouteMetric: 7, RouteTable: tc.profileTable}
			if err := r.Set(cfg); err != nil {
				t.Fatal(err)
			}
			if r.routeTableMain != (tc.wantTable == "main") || r.routeMetric != tc.wantMetric {
				t.Fatalf("table main=%v, metric=%v; want table=%q, metric=%v", r.routeTableMain, r.routeMetric, tc.wantTable, tc.wantMetric)
			}
			if tc.wantTable == "main" && len(os.rules) != 0 {
				t.Fatalf("main override installed policy rules: %v", os.rules)
			}
			if cfg.RouteTable != tc.profileTable || cfg.RouteMetric != 7 {
				t.Fatal("modified the caller's router configuration")
			}
			if len(os.routes) != 1 || !strings.HasSuffix(os.routes[0], "metric "+strconv.Itoa(tc.wantMetric)) && tc.wantMetric != 0 {
				t.Fatalf("incorrect route: %v", os.routes)
			}
		})
	}
}

func TestLinuxRouteServiceOverridesInvalid(t *testing.T) {
	for _, tc := range []struct{ table, metric string }{
		{"100", "4096"}, {"main", "-1"}, {"main", "2147483648"}, {"main", "abc"},
	} {
		t.Run(tc.table+"/"+tc.metric, func(t *testing.T) {
			envknob.SetenvForTest(t, "TS_ROUTE_TABLE", tc.table)
			envknob.SetenvForTest(t, "TS_ROUTE_METRIC", tc.metric)
			if _, _, err := linuxRouteOverridesFromEnv(); err == nil {
				t.Fatal("accepted invalid service routing settings")
			}
		})
	}
}

type failRouteDelete struct {
	*fakeOS
	fail bool
}

func (c *failRouteDelete) run(args ...string) error {
	if c.fail && len(args) > 2 && args[1] == "route" && args[2] == "del" {
		return errors.New("injected route deletion failure")
	}
	return c.fakeOS.run(args...)
}

func TestLinuxRoutePreferenceCleanupRetry(t *testing.T) {
	r, os := newTestLinuxRouter(t)
	cfg := &Config{Routes: mustCIDRs("192.168.16.0/24"), RouteMetric: 4096}
	if err := r.Set(cfg); err != nil {
		t.Fatal(err)
	}
	cmd := &failRouteDelete{fakeOS: os, fail: true}
	r.cmd = cmd
	cfg.RouteTable, cfg.RouteMetric = "main", 8192
	if err := r.Set(cfg); err == nil {
		t.Fatal("Set succeeded despite cleanup failure")
	}
	if r.routeTableMain || r.routeMetric != 4096 || len(r.routes) != 1 {
		t.Fatal("lost old route settings after failed cleanup")
	}
	cmd.fail = false
	if err := r.Set(cfg); err != nil {
		t.Fatal(err)
	}
	if len(os.routes) != 1 || os.routes[0] != "192.168.16.0/24 dev tailscale0 table main metric 8192" {
		t.Fatalf("retry did not remove old route: %v", os.routes)
	}
}

func TestLinuxNetlinkRouteMetric(t *testing.T) {
	for _, table := range []bool{false, true} {
		for _, metric := range []int{0, 4096, 1<<31 - 1} {
			r := &linuxRouter{ipRuleAvailable: true, routeTableMain: table, routeMetric: metric}
			for _, prefix := range mustCIDRs("192.168.16.0/24", "fd7a:115c:a1e0::/64") {
				route := r.netlinkRoute(prefix, 7, unix.RTN_UNICAST)
				wantMetric, wantTable := metric, 52
				if metric == 0 && prefix.Addr().Is6() {
					wantMetric = 1024
				}
				if table {
					wantTable = 254
				}
				if route.Priority != wantMetric || route.Table != wantTable || route.LinkIndex != 7 {
					t.Fatalf("route %v: %+v", prefix, route)
				}
			}
		}
	}
	// Kernels without policy routing still receive the configured metric.
	r := &linuxRouter{routeMetric: 4096}
	args := r.appendRouteArgs(nil, netip.MustParsePrefix("192.168.16.0/24"))
	if !slices.Equal(args, []string{"metric", "4096"}) {
		t.Fatalf("without policy routing: %v", args)
	}
}

// Exercise the real kernel in a private network namespace, so even root test
// runs never touch the host's routes. Also run this with TS_DEBUG_USE_IP_COMMAND
// to cover the fallback command path's conflict checks.
func TestMainTableRouteCoexistence(t *testing.T) {
	tstest.RequireRoot(t)
	runtime.LockOSThread()
	defer runtime.UnlockOSThread()
	original, err := netns.Get()
	if err != nil {
		t.Fatal(err)
	}
	defer original.Close()
	ns, err := netns.New()
	if err != nil {
		t.Skipf("network namespaces unavailable: %v", err)
	}
	defer ns.Close()
	defer func() {
		if err := netns.Set(original); err != nil {
			t.Fatal(err)
		}
	}()
	for _, name := range []string{"lan-test", "tailscale0"} {
		if err := netlink.LinkAdd(&netlink.Dummy{LinkAttrs: netlink.LinkAttrs{Name: name}}); err != nil {
			t.Fatal(err)
		}
		link, err := netlink.LinkByName(name)
		if err != nil {
			t.Fatal(err)
		}
		if err := netlink.LinkSetUp(link); err != nil {
			t.Fatal(err)
		}
	}
	lan, err := netlink.LinkByName("lan-test")
	if err != nil {
		t.Fatal(err)
	}
	r := &linuxRouter{
		logf: t.Logf, tunname: "tailscale0", cmd: osCommandRunner{},
		nfr: newIPTablesRunner(t), interfaceV6Usable: func() bool { return true },
		ipRuleAvailable: true, routeTableMain: true, routeMetric: 4096,
	}
	for _, addr := range mustCIDRs("192.168.16.1/24", "fd00:16::1/64") {
		a := nlAddrOfPrefix(addr)
		a.Flags = unix.IFA_F_NODAD
		if err := netlink.AddrAdd(lan, a); err != nil {
			t.Fatal(err)
		}
		prefix := addr.Masked()
		if err := r.addRoute(prefix); err != nil {
			t.Fatal(err)
		}
		// A restart must adopt its own identical route, without replacing LAN.
		if err := r.addRoute(prefix); err != nil {
			t.Fatalf("adopting existing Tailscale route: %v", err)
		}
		peer := addr.Addr().Next()
		routes, err := netlink.RouteGet(peer.AsSlice())
		if err != nil || len(routes) == 0 {
			t.Fatalf("route lookup for %v: %v, %v", peer, routes, err)
		}
		if routes[0].LinkIndex != lan.Attrs().Index {
			t.Fatalf("%v routed through Tailscale instead of LAN: %v", peer, routes)
		}
		for range 2 {
			if err := r.delRoute(prefix); err != nil {
				t.Fatal(err)
			}
		}
		remaining, err := netlink.RouteListFiltered(netlink.FAMILY_ALL,
			&netlink.Route{Table: 254, Dst: netipx.PrefixIPNet(prefix)},
			netlink.RT_FILTER_TABLE|netlink.RT_FILTER_DST)
		if err != nil || len(remaining) != 1 || remaining[0].LinkIndex != lan.Attrs().Index {
			t.Fatalf("LAN route changed after deletion: %v, %v", remaining, err)
		}
		if addr.Addr().Is4() {
			// IPv4 cannot add a different interface at the same prefix/metric.
			// Report the conflict rather than replacing the connected LAN route.
			r.routeMetric = remaining[0].Priority
			if err := r.addRoute(prefix); err == nil {
				t.Fatal("overwrote/adopted another interface's conflicting route")
			}
			r.routeMetric = 4096
		}
	}
	// Exercise actual policy rule deletion and metric-specific cleanup when
	// moving from the default table (including IPv6 metric zero) into main.
	r.ipPolicyPrefBase = 5200
	for _, cfg := range []*Config{
		{RouteMetric: 0},
		{RouteTable: "main", RouteMetric: 4096},
	} {
		if err := r.reconfigureRouting(cfg); err != nil {
			t.Fatal(err)
		}
		for _, prefix := range mustCIDRs("192.168.16.0/24", "fd00:16::/64") {
			if err := r.addRoute(prefix); err != nil {
				t.Fatal(err)
			}
			r.routes[prefix] = true
		}
		for _, prefix := range mustCIDRs("10.0.0.0/8", "fe80::/64") {
			if err := r.addThrowRoute(prefix); err != nil {
				t.Fatal(err)
			}
			r.localRoutes[prefix] = true
		}
		rules, err := netlink.RuleList(netlink.FAMILY_ALL)
		if err != nil {
			t.Fatal(err)
		}
		count := 0
		for _, rule := range rules {
			if rule.Priority >= 5200 && rule.Priority < 5300 {
				count++
			}
		}
		if (count == 0) != (cfg.RouteTable == "main") {
			t.Fatalf("policy rules for table %q: %v", cfg.RouteTable, rules)
		}
	}
	if err := r.delRoutes(); err != nil {
		t.Fatal(err)
	}
	for _, prefix := range mustCIDRs("192.168.16.0/24", "fd00:16::/64") {
		routes, err := netlink.RouteListFiltered(netlink.FAMILY_ALL,
			&netlink.Route{Dst: netipx.PrefixIPNet(prefix)},
			netlink.RT_FILTER_TABLE|netlink.RT_FILTER_DST)
		if err != nil || len(routes) != 1 || routes[0].LinkIndex != lan.Attrs().Index {
			t.Fatalf("switching table left stale routes or changed LAN: %v, %v", routes, err)
		}
	}
}
