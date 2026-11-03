// Package report makes result reporting reliable when the command center or
// the network is down for a while.
//
// A result is first given a UUID, then written to a durable local spool, and
// only then sent to the command center in order. The spool is an transactional SQLite
// database with a size and age cap (oldest-first drop, logged), so a restart
// resumes exactly where it stopped. Because the UUID is allocated before
// spooling, a replayed result is idempotent at the center under the node-scoped UUID ingestion contract. Transient failures are retried with exponential backoff and
// jitter; the backlog is flushed in batches once the center is reachable.
package report
