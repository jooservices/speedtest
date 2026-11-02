# Linux multi-uplink routing

The agent can bind a test to one local interface or one local source IP. It
does not add routes or change host networking. The selected interface must
exist, be up, and have an IP address; a selected source IP must be assigned to
an up local interface. With neither value configured, the agent uses the
system default route and reports `network_path="default route"` in its status.

Choose exactly one setting in the node's `config.yaml`:

```yaml
# Bind by local interface name:
network_interface: enp1s0

# Or bind by local source address instead:
# source_ip: 192.0.2.10
```

Ookla receives `--interface <name>` or `--ip <address>`. The agent's
`GET /api/agent/ip` request uses the same binding and records the address seen
by the command center as the run's public IP. The engine-reported IP is kept as
a cross-check; a difference is recorded in the result's `warnings` field.

## Prerequisites

- Linux with `iproute2` (`ip` command).
- Each uplink is configured with its own interface address and reachable
  gateway. Replace the example names and addresses below with the values for
  the host; the documentation-only addresses are not usable on a real network.
- Separate policy-routing tables and source rules for uplinks whose replies
  must leave through their matching gateway. `ip rule` selects a routing table
  by source prefix; `ip route` supplies that table's connected and default
  routes. See [`ip-rule(8)`](https://man7.org/linux/man-pages/man8/ip-rule.8.html)
  and [`ip-route(8)`](https://man7.org/linux/man-pages/man8/ip-route.8.html).
- Check reverse-path filtering on multi-homed hosts. Strict mode can discard
  valid asymmetric traffic. If the network requires asymmetric return paths,
  use loose mode (`2`) on the affected interfaces; keep `all` unchanged unless
  loose mode is required globally. Do not disable filtering without a separate
  security review. Linux uses the maximum of the `all` and per-interface
  `rp_filter` values. See the
  [Linux kernel IP sysctl documentation](https://www.kernel.org/doc/html/latest/networking/ip-sysctl.html).

## Two uplinks

First replace all example values with the host's interface, source address,
subnet, and gateway values. Run this from a root shell or prefix `ip` commands
with `sudo`:

```bash
configure_uplink() {
  priority="$1" table="$2" iface="$3" source="$4" subnet="$5" gateway="$6"
  ip route replace "$subnet" dev "$iface" src "$source" table "$table"
  ip route replace default via "$gateway" dev "$iface" table "$table"
  ip rule del priority "$priority" from "$source/32" lookup "$table" 2>/dev/null || true
  ip rule add priority "$priority" from "$source/32" lookup "$table"
}

configure_uplink 101 101 enp1s0 192.0.2.10 192.0.2.0/24 192.0.2.1
configure_uplink 102 102 enp2s0 198.51.100.10 198.51.100.0/24 198.51.100.1
```

Then set the node's `network_interface` or `source_ip` to the matching value.
The rule priority must be unique and lower than the normal `main` rule (32766).

## Four uplinks

Assign a distinct table and priority to each uplink. Replace each example
value before running this standalone block:

```bash
configure_uplink() {
  priority="$1" table="$2" iface="$3" source="$4" subnet="$5" gateway="$6"
  ip route replace "$subnet" dev "$iface" src "$source" table "$table"
  ip route replace default via "$gateway" dev "$iface" table "$table"
  ip rule del priority "$priority" from "$source/32" lookup "$table" 2>/dev/null || true
  ip rule add priority "$priority" from "$source/32" lookup "$table"
}

configure_uplink 101 101 enp1s0 192.0.2.10       192.0.2.0/24       192.0.2.1
configure_uplink 102 102 enp2s0 198.51.100.10   198.51.100.0/24   198.51.100.1
configure_uplink 103 103 enp3s0 203.0.113.10    203.0.113.0/24    203.0.113.1
configure_uplink 104 104 enp4s0 198.18.0.10     198.18.0.0/24     198.18.0.1
```

For IPv6, use `ip -6 route` and `ip -6 rule`, each source as a `/128`, and the
actual IPv6 prefix and gateway for that uplink. Do not copy the IPv4 examples
unchanged for IPv6.

If reverse-path filtering must be loose for asymmetric paths, inspect current
values first and apply the change only to the affected uplinks:

```bash
sysctl net.ipv4.conf.all.rp_filter
sysctl net.ipv4.conf.enp1s0.rp_filter
sysctl net.ipv4.conf.enp2s0.rp_filter
sysctl -w net.ipv4.conf.enp1s0.rp_filter=2
sysctl -w net.ipv4.conf.enp2s0.rp_filter=2
```

Persist approved values through the host's normal `/etc/sysctl.d/` process.
This changes host policy and is intentionally outside the agent.

## Verify routing and agent status

For each uplink, `ip route get` must show its selected gateway, table, and
source address. Replace the example source with an address configured above:

```bash
ip -4 route get 1.1.1.1 from 192.0.2.10
```

The output should include the expected `via`, `dev`, `table`, and `src` values.
Also inspect `ip rule show` and `ip route show table 101` (repeat for each
table). On agent startup, the structured status log reports the selected
interface/source IP or `network_path="default route"` when unbound.

For Linux namespace verification of actual selected-interface egress, run
`scripts/test-netpath-namespace.sh` as root from the repository. It creates a
temporary agent and center namespace pair, checks the address observed by the
center echo endpoint, and removes both namespaces on exit.
