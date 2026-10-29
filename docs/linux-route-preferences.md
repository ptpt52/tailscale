# Linux routing table and metric

This fork supports `--route-table` and `--route-metric` on `tailscale up`,
`tailscale login`, and `tailscale set` on Linux. Both settings are saved in the
current profile and reused when tailscaled starts.

To keep connected LAN routes ahead of accepted Tailscale routes with the same
prefix, put Tailscale routes in the main table at a higher metric:

```sh
tailscale set --route-table=main --route-metric=4096
```

For initial configuration:

```sh
tailscale up --accept-routes --route-table=main --route-metric=4096
```

`main` installs explicitly managed IPv4 and IPv6 routes in table 254. It does
not install Tailscale policy routing rules or throw routes. Rules left by the
default mode are removed during startup and when switching to main. Existing
LAN/WAN routes are preserved; a same-prefix, same-metric IPv4 conflict is
reported rather than replacing another interface's route.

The metric must be between 0 and 2147483647. Zero uses the kernel default:
0 for IPv4 and 1024 for IPv6. Choose a value higher than the relevant LAN route
metric. Every explicitly managed tunnel route uses this metric; in table 52,
throw routes also use it. Kernel-generated routes for local interface addresses
are not changed.

Metric compares routes with the **same prefix** in the same table. A more
specific Tailscale prefix still wins over a less specific LAN prefix. Main mode
also removes the policy rules that normally bypass Tailscale for its transport
packets and implement exit-node LAN exclusions. It therefore changes exit-node
behavior: a higher-metric Tailscale default route loses to the existing WAN
default route, and a route covering a transport endpoint can create a routing
loop. Use the default table mode when those policy-routing semantics are needed.

To restore the default routing settings:

```sh
tailscale set --route-table=52 --route-metric=0
```

Changing either setting removes previously managed routes using their old
table and metric, then installs the desired routes with the new values. This
can briefly interrupt Tailscale traffic. Native LAN/WAN routes are not flushed.

## Service startup settings

Services that manage routing configuration independently of the saved profile
can set `TS_ROUTE_TABLE` and `TS_ROUTE_METRIC` in tailscaled's environment:

```sh
TS_ROUTE_TABLE=main TS_ROUTE_METRIC=4096 tailscaled
```

These settings apply before the first routes are installed. This avoids a
window where a saved profile could install table 52 policy rules before a
later `tailscale set` command changes them. The accepted values are `52` or
`main` for the table and 0 through 2147483647 for the metric. Invalid values
prevent the Linux router from starting.

Each unset variable leaves that profile preference in control. A set variable
overrides the corresponding profile preference for the lifetime of the daemon;
it does not rewrite the saved preference shown by `tailscale get`. Restart the
daemon to change service settings. OpenWrt's init script supplies both values
from `/etc/config/tailscale`.
