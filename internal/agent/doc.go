// Package agent implements the speed-test node agent. It can be enrolled once
// against a command center, then run as a background service. A node knows only
// its own configuration and never what it is attached to.
//
// The sub-packages hold the parts built so far: config loads the node's own
// configuration, and enroll exchanges an enrollment token for the node
// credential. Running speed tests, reporting results and scheduling are not
// implemented yet.
package agent
