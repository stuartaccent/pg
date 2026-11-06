package web

import (
	"bytes"
	"context"
	"strings"
	"testing"

	"pg/internal/session"
)

const analyzed = `[{"Plan": {
	"Node Type": "Hash Join", "Join Type": "Inner", "Total Cost": 100.5, "Plan Rows": 10,
	"Actual Total Time": 50, "Actual Rows": 5000, "Actual Loops": 1, "Hash Cond": "(a.id = b.id)",
	"Plans": [
		{"Node Type": "Seq Scan", "Relation Name": "orders", "Alias": "a", "Total Cost": 60, "Plan Rows": 2000,
		 "Actual Total Time": 10, "Actual Rows": 2000, "Actual Loops": 2, "Filter": "(x > 1)"},
		{"Node Type": "Index Scan", "Index Name": "b_pkey", "Relation Name": "b", "Total Cost": 8, "Plan Rows": 1000,
		 "Actual Total Time": 0, "Actual Rows": 0, "Actual Loops": 0}
	]}, "Planning Time": 0.25, "Execution Time": 51.5}]`

func TestParsePlan(t *testing.T) {
	p, err := parsePlan(analyzed)
	if err != nil {
		t.Fatal(err)
	}
	root := p.Root
	scan, index := root.Children[0], root.Children[1]

	// The join's 50ms includes the scan's 20, which is 10 over two loops.
	if root.own != 30 || scan.own != 20 || index.own != 0 {
		t.Errorf("own = %v %v %v", root.own, scan.own, index.own)
	}
	if root.Share != 0.6 || scan.Share != 0.4 {
		t.Errorf("share = %v %v", root.Share, scan.Share)
	}
	if !root.Slowest || scan.Slowest {
		t.Errorf("slowest = %v %v", root.Slowest, scan.Slowest)
	}
	if !scan.SeqScan || root.SeqScan {
		t.Errorf("seq scan = %v %v", scan.SeqScan, root.SeqScan)
	}
	if scan.Target != "orders as a" || index.Target != "b_pkey on b" {
		t.Errorf("targets = %q, %q", scan.Target, index.Target)
	}
	if root.Estimate != "500× more rows than estimated" || scan.Estimate != "" {
		t.Errorf("estimates = %q, %q", root.Estimate, scan.Estimate)
	}
	if !strings.Contains(index.Stats, "never run") {
		t.Errorf("stats = %q", index.Stats)
	}
	if p.Timing != "Planning 0.25 ms · Execution 51.5 ms" {
		t.Errorf("timing = %q", p.Timing)
	}
	if len(scan.Details) != 1 || scan.Details[0] != (planDetail{"Filter", "(x > 1)"}) {
		t.Errorf("details = %+v", scan.Details)
	}
}

func TestParsePlanWithoutRunning(t *testing.T) {
	p, err := parsePlan(`[{"Plan": {"Node Type": "Sort", "Total Cost": 40, "Plan Rows": 3, "Sort Key": ["a", "b"],
		"Plans": [{"Node Type": "Seq Scan", "Relation Name": "t", "Total Cost": 10, "Plan Rows": 3}]}}]`)
	if err != nil {
		t.Fatal(err)
	}
	if p.Root.Share != 0.75 || !p.Root.Slowest || p.Timing != "" || p.Root.Estimate != "" {
		t.Errorf("plan = %+v", p.Root)
	}
	if p.Root.Details[0].Value != "a, b" {
		t.Errorf("details = %+v", p.Root.Details)
	}
}

func TestLimitExplainsFewerRows(t *testing.T) {
	p, err := parsePlan(`[{"Plan": {"Node Type": "Limit", "Plan Rows": 5, "Actual Rows": 5, "Actual Loops": 1, "Actual Total Time": 2,
		"Plans": [{"Node Type": "Sort", "Plan Rows": 2000, "Actual Rows": 5, "Actual Loops": 1, "Actual Total Time": 2,
			"Plans": [{"Node Type": "Seq Scan", "Plan Rows": 1, "Actual Rows": 2000, "Actual Loops": 1, "Actual Total Time": 1}]}]}}]`)
	if err != nil {
		t.Fatal(err)
	}
	sort := p.Root.Children[0]
	if sort.Estimate != "" {
		t.Errorf("sort = %q", sort.Estimate)
	}
	if sort.Children[0].Estimate == "" {
		t.Error("more rows than estimated still counts under a limit")
	}
}

func TestEstimateMiss(t *testing.T) {
	for _, tt := range []struct {
		estimated, actual float64
		want              string
	}{
		{100, 100, ""},
		{100, 11, ""},
		{100, 10, "10× fewer rows than estimated"},
		{1, 0, ""},
		{0, 9, ""},
		{2, 20, "10× more rows than estimated"},
	} {
		if got := estimateMiss(tt.estimated, tt.actual); got != tt.want {
			t.Errorf("%v, %v: got %q", tt.estimated, tt.actual, got)
		}
	}
}

func TestPlanThatWontParseStillShows(t *testing.T) {
	if _, err := parsePlan(`[]`); err == nil {
		t.Error("empty: want an error")
	}
	var b bytes.Buffer
	if err := Results(session.Page{Done: true, Plan: "not json"}, "1").Render(context.Background(), &b); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(b.String(), "not json") {
		t.Errorf("got %s", b.String())
	}
}

// The row asking for the next result stays as the table's summary, so what
// comes back has to land after the table rather than replace the row.
func TestEndedResultAsksForTheNext(t *testing.T) {
	var b bytes.Buffer
	p := session.Page{QueryID: 7, Columns: []string{"a"}, NumCols: 1, Total: 1, Ended: true, Rows: [][]session.Value{{{Text: "1"}}}}
	if err := Results(p, "t 1").Render(context.Background(), &b); err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{`/query/7/more?tab=t+1&amp;next=1`, `hx-target="closest table"`, `hx-swap="afterend"`, "1 rows"} {
		if !strings.Contains(b.String(), want) {
			t.Errorf("missing %s in %s", want, b.String())
		}
	}
}
