package catalog

import (
	"context"
	"slices"
	"strings"
	"testing"
)

func TestHealth(t *testing.T) {
	c := newTestCatalog(t)
	ctx := context.Background()
	for _, sql := range []string{
		"create temp table health_parent (id int primary key)",
		"create temp table health_child (id int, parent_id int references health_parent)",
		"create index health_a on health_child (id)",
		"create index health_b on health_child (id)",
	} {
		if _, err := c.read(ctx, sql, nil); err != nil {
			t.Fatal(err)
		}
	}

	report, err := c.Health(ctx)
	if err != nil {
		t.Fatal(err)
	}
	found := map[string]Check{}
	for _, check := range report.Checks {
		if check.Err != nil {
			t.Errorf("%s: %v", check.Name, check.Err)
		}
		if check.Reason == "" || (len(check.Rows) > 0 && len(check.Columns) != len(check.Rows[0])) {
			t.Errorf("%s: reason %q, columns %v", check.Name, check.Reason, check.Columns)
		}
		found[check.Name] = check
	}
	if len(found) != 7 {
		t.Errorf("checks = %v", slices.Collect(func(yield func(string) bool) {
			for name := range found {
				yield(name)
			}
		}))
	}

	has := func(check, want string) bool {
		for _, row := range found[check].Rows {
			if strings.Contains(strings.Join(row, " "), want) {
				return true
			}
		}
		return false
	}
	if !has("Unindexed foreign keys", "health_child_parent_id_fkey") {
		t.Errorf("foreign key not found: %v", found["Unindexed foreign keys"].Rows)
	}
	if has("Unindexed foreign keys", "health_child_id") {
		t.Error("listed a foreign key twice")
	}
	if !has("Duplicate indexes", "health_a, health_b") {
		t.Errorf("duplicates not found: %v", found["Duplicate indexes"].Rows)
	}
	if !found["Unused indexes"].Usage || found["Duplicate indexes"].Usage {
		t.Error("usage flags wrong")
	}
	if strings.HasPrefix(found["Unused indexes"].Reason, "Usage") {
		t.Errorf("reason = %q", found["Unused indexes"].Reason)
	}

	// A unique index on some of its columns answers it.
	for _, sql := range []string{
		"create temp table health_pair (id int, kind text, primary key (id, kind))",
		"create temp table health_pair_child (pair_id int unique, pair_kind text, foreign key (pair_id, pair_kind) references health_pair)",
	} {
		if _, err := c.read(ctx, sql, nil); err != nil {
			t.Fatal(err)
		}
	}

	// So does one starting with its first column, or leaving out nulls.
	for _, sql := range []string{
		"create temp table health_lead (pair_id int, pair_kind text, foreign key (pair_id, pair_kind) references health_pair)",
		"create index on health_lead (pair_id)",
		"create temp table health_partial (parent_id int references health_parent)",
		"create index on health_partial (parent_id) where parent_id is not null",
		"create temp table health_other (parent_id int references health_parent, flag bool)",
		"create index on health_other (parent_id) where flag",
		"create temp table health_second (pair_id int, pair_kind text, foreign key (pair_id, pair_kind) references health_pair)",
		"create index on health_second (pair_kind)",
	} {
		if _, err := c.read(ctx, sql, nil); err != nil {
			t.Fatal(err)
		}
	}

	// An index on the foreign key's columns, first, answers it.
	if _, err := c.read(ctx, "create index health_c on health_child (parent_id, id)", nil); err != nil {
		t.Fatal(err)
	}
	if report, err = c.Health(ctx); err != nil {
		t.Fatal(err)
	}
	for _, check := range report.Checks {
		if check.Name == "Unindexed foreign keys" {
			found[check.Name] = check
		}
	}
	if has("Unindexed foreign keys", "health_child_parent_id_fkey") {
		t.Error("still listed with an index")
	}
	if has("Unindexed foreign keys", "health_pair_child") {
		t.Error("listed with a unique index on part of it")
	}
	if has("Unindexed foreign keys", "health_lead") {
		t.Error("listed with an index on its first column")
	}
	if has("Unindexed foreign keys", "health_partial") {
		t.Error("listed with an index leaving out nulls")
	}
	if !has("Unindexed foreign keys", "health_other") {
		t.Error("an index on other rows answered it")
	}
	if !has("Unindexed foreign keys", "health_second") {
		t.Error("an index on its second column answered it")
	}
}

func TestNewCheck(t *testing.T) {
	check := newCheck("5 Unused indexes.sql", "-- Usage: never scanned.\n--  Second line.\nselect 1 -- not this\n-- nor this")
	if check.Name != "Unused indexes" || check.Reason != "Never scanned. Second line." || !check.Usage {
		t.Errorf("check = %+v", check)
	}
}
