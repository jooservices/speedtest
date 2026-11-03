// Package agent implements the speed-test node agent. It can be enrolled once
// against a command center, then run as a background service. A node knows only
// its own configuration and never what it is attached to.
//
// The sub-packages hold the parts built so far: config loads the node's own
// configuration, enroll exchanges an enrollment token for the node credential,
// engine executes speed tests, sched runs them on the node's local schedule and
// control heartbeats, syncs configuration and acknowledges commands from the
// center. Reporting results to the center is not implemented yet.
package agent
