package catalog

import (
	"context"
	"errors"
	"strconv"
	"time"
)

// Backend is one connection to the server, as pg_stat_activity shows it. What
// a role isn't allowed to see of others' connections comes through empty.
type Backend struct {
	PID       int
	User      string
	Database  string
	App       string
	State     string
	For       string // how long it's been in that state; open transactions count from their start
	BlockedBy string // pids holding the lock it's waiting for
	Query     string
}

// The ones with something happening come first, longest running at the top.
const activityQuery = `
	select a.pid::text, a.usename, a.datname, a.application_name, coalesce(a.state, a.backend_type),
		extract(epoch from clock_timestamp() - s.since)::bigint::text,
		case when a.wait_event_type = 'Lock' then array_to_string(pg_blocking_pids(a.pid), ', ') end,
		a.query
	from pg_stat_activity a
	cross join lateral (
		select case
			when a.state = 'active' then a.query_start
			when a.state like 'idle in%' then a.xact_start
			else a.state_change
		end since
	) s
	where a.pid <> pg_backend_pid() and a.backend_type in ('client backend', 'autovacuum worker')
	order by a.state = 'idle', s.since`

// Activity returns the server's connections, for every database, leaving out
// the one asking.
func (c *Catalog) Activity(ctx context.Context) ([]Backend, error) {
	rows, err := c.read(ctx, activityQuery, nil)
	if err != nil {
		return nil, err
	}
	backends := make([]Backend, len(rows))
	for i, r := range rows {
		pid, err := strconv.Atoi(string(r[0]))
		if err != nil {
			return nil, err
		}
		backends[i] = Backend{
			PID:       pid,
			User:      string(r[1]),
			Database:  string(r[2]),
			App:       string(r[3]),
			State:     string(r[4]),
			BlockedBy: string(r[6]),
			Query:     string(r[7]),
		}
		if seconds, err := strconv.Atoi(string(r[5])); err == nil {
			backends[i].For = (time.Duration(seconds) * time.Second).String()
		}
	}
	return backends, nil
}

// Stop cancels what pid is running or, with terminate, ends the connection.
func (c *Catalog) Stop(ctx context.Context, pid int, terminate bool) error {
	sql := "select pg_cancel_backend($1)"
	if terminate {
		sql = "select pg_terminate_backend($1)"
	}
	rows, err := c.read(ctx, sql, [][]byte{[]byte(strconv.Itoa(pid))})
	if err != nil {
		return err
	}
	if string(rows[0][0]) != "t" {
		return errors.New("that connection has already gone")
	}
	return nil
}
