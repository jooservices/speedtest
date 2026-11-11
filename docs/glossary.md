# Glossary

Short definitions of the terms Speedtest uses. They match the OpenAPI contract
([openapi.yaml](openapi.yaml)) and the UI preview; where a term has a schema
there, the schema is the source of truth.

## Node

One machine running the agent. A node is independent: it knows only its own
configuration — name, expected download/upload speed, schedule, parallel
connections (threads) and the server to test against. It never knows or cares
what it is attached to (a single ISP line, a load balancer, a VPN); "WAN" is
just a label the owner chooses. Schema: `Node`.

## Result

One speed test measurement taken by a node and stored by the command center.
A result records when the test ran (`ts`), its status (`ok`, `degraded` or
`failed`), the measured download/upload in Mbps, ping and jitter in
milliseconds, packet loss, the public IP the test used, and the engine details
and raw output. Unavailable measurements are `null`, never zero. Schema:
`Result`.

## Expected speed

The download/upload speed a node is provisioned to receive, in Mbps, kept as
part of the node's configuration (`expectedDown`/`expectedUp`). A result is
judged against it as "percent of expected", which is the fair way to compare
nodes of different capacity. Percent of expected can exceed 100.

## IP history

The record of every public IP a node used and when it changed. For each address
it stores the first and last time it was seen, how many tests used it, its
average download/upload, and its geolocation. Address changes are tracked as
transitions (`from` → `to`) and carry the ISP of each address when known.
Schemas: `IPAddress`, `IPChange`.

## ISP

An Internet Service Provider entity inferred from the ASN number and
organisation already stored with each public IP's geolocation. Results and
addresses link to an ISP; a node exposes its current ISP; optional plans hold
advertised down/up speeds used as expected-speed defaults. Schema: `ISP`.

## Rule

An alert rule — a condition the center checks against every new result. A rule
names a metric (`down_pct`, `up_pct`, `ping`, `jitter`, `loss`, `failed`,
`ip_changed` or `offline`), a comparison operator and threshold, how many tests
in a row must match (`forTests`), which nodes it applies to, a severity
(`info`, `warn` or `crit`) and which notification channels to use. Schema:
`AlertRule`.

## Event

An alert event — an occurrence the center creates when a rule's condition holds
for its streak, or when a node goes offline. An event has a severity, a
timestamp, the node and rule that produced it, a title and detail, an optional
resolution time, and an acknowledgement flag that is independent of resolution.
One event is created per streak; it does not fire again while the streak is
still open. Schema: `AlertEvent`.
