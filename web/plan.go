package web

import (
	"bytes"
	"encoding/json"
	"fmt"
	"maps"
	"slices"
	"strconv"
	"strings"
)

// planNode is one step of a query plan, ready to show.
type planNode struct {
	Type     string
	Target   string // the table, index or CTE it works on
	Stats    string
	Share    float64 // of the whole plan's time, or cost if it wasn't run, from 0 to 1
	Slowest  bool
	SeqScan  bool
	Estimate string // says how far out the row estimate was, if it was badly
	Details  []planDetail
	Children []*planNode

	whole float64 // time or cost, including the steps below it
	own   float64 // the same, leaving them out
}

type planDetail struct{ Name, Value string }

// plan is the output of explain (format json).
type plan struct {
	Root   *planNode
	Timing string
}

// shown are the fields planNode already has a place for, kept out of Details.
var shown = []string{
	"Node Type", "Relation Name", "Alias", "Index Name", "CTE Name", "Function Name", "Subplan Name", "Plans",
	"Startup Cost", "Total Cost", "Plan Rows", "Plan Width",
	"Actual Startup Time", "Actual Total Time", "Actual Rows", "Actual Loops",
}

// parsePlan reads a plan. Numbers stay as written, as float64 would print big
// ones in exponent form.
func parsePlan(raw string) (plan, error) {
	var out []struct {
		Plan      map[string]any
		Planning  *json.Number `json:"Planning Time"`
		Execution *json.Number `json:"Execution Time"`
	}
	d := json.NewDecoder(strings.NewReader(raw))
	d.UseNumber()
	if err := d.Decode(&out); err != nil {
		return plan{}, err
	}
	if len(out) != 1 || out[0].Plan == nil {
		return plan{}, fmt.Errorf("not a query plan")
	}
	root := newPlanNode(out[0].Plan, false)
	total, slowest := 0.0, root
	walkPlan(root, func(n *planNode) {
		total += n.own
		if n.own > slowest.own {
			slowest = n
		}
	})
	if total > 0 {
		walkPlan(root, func(n *planNode) { n.Share = n.own / total })
		slowest.Slowest = true
	}
	p := plan{Root: root}
	if out[0].Planning != nil && out[0].Execution != nil {
		p.Timing = fmt.Sprintf("Planning %s ms · Execution %s ms", *out[0].Planning, *out[0].Execution)
	}
	return p, nil
}

func walkPlan(n *planNode, visit func(*planNode)) {
	visit(n)
	for _, child := range n.Children {
		walkPlan(child, visit)
	}
}

// newPlanNode builds the step m and those below it. limited says it's under a
// Limit, which stops it early, so fewer rows than estimated is only to be expected.
func newPlanNode(m map[string]any, limited bool) *planNode {
	n := &planNode{Type: text(m, "Node Type")}
	n.SeqScan = strings.Contains(n.Type, "Seq Scan")
	n.Target = planTarget(m)

	_, ran := m["Actual Loops"]
	loops := number(m, "Actual Loops")
	estimated := number(m, "Plan Rows")
	cost := number(m, "Total Cost")
	// Actual figures are averages per loop, so the whole is the average times
	// the loops. Parallel workers overlap, which makes this only a guide.
	switch {
	case ran && loops == 0:
		n.Stats = fmt.Sprintf("never run · cost %.2f", cost)
	case ran:
		rows := number(m, "Actual Rows")
		n.whole = number(m, "Actual Total Time") * loops
		n.Stats = fmt.Sprintf("%.3f ms · %s rows", n.whole, rowCount(rows*loops))
		if loops > 1 {
			n.Stats += fmt.Sprintf(" · %.0f loops", loops)
		}
		n.Stats += fmt.Sprintf(" · estimated %s · cost %.2f", rowCount(estimated*loops), cost)
		if !limited || rows > estimated {
			n.Estimate = estimateMiss(estimated, rows)
		}
	default:
		n.whole = cost
		n.Stats = fmt.Sprintf("%s rows · cost %.2f", rowCount(estimated), cost)
	}

	children, _ := m["Plans"].([]any)
	below := 0.0
	for _, c := range children {
		if child, ok := c.(map[string]any); ok {
			node := newPlanNode(child, limited || n.Type == "Limit")
			n.Children = append(n.Children, node)
			below += node.whole
		}
	}
	n.own = max(n.whole-below, 0)

	for _, name := range slices.Sorted(maps.Keys(m)) {
		// Most of what a step reports is a zero, which only hides the rest.
		if value := planValue(m[name]); !slices.Contains(shown, name) && value != "0" && value != "false" {
			n.Details = append(n.Details, planDetail{name, value})
		}
	}
	return n
}

func planTarget(m map[string]any) string {
	target := text(m, "Relation Name")
	if alias := text(m, "Alias"); target != "" && alias != "" && alias != target {
		target += " as " + alias
	}
	if index := text(m, "Index Name"); index != "" {
		if target == "" {
			return index
		}
		return index + " on " + target
	}
	return strings.Join(slices.DeleteFunc([]string{target, text(m, "CTE Name"), text(m, "Function Name"), text(m, "Subplan Name")}, func(s string) bool { return s == "" }), " ")
}

// estimateMiss describes a row estimate out by 10 times or more, as that's when
// the planner tends to choose badly. A row or fewer counts as one, or every
// empty result would be infinitely wrong.
func estimateMiss(estimated, actual float64) string {
	e, a := max(estimated, 1), max(actual, 1)
	switch {
	case a/e >= 10:
		return fmt.Sprintf("%.0f× more rows than estimated", a/e)
	case e/a >= 10:
		return fmt.Sprintf("%.0f× fewer rows than estimated", e/a)
	}
	return ""
}

func rowCount(rows float64) string { return strconv.FormatFloat(rows, 'f', -1, 64) }

func text(m map[string]any, key string) string {
	s, _ := m[key].(string)
	return s
}

func number(m map[string]any, key string) float64 {
	n, _ := m[key].(json.Number)
	f, _ := n.Float64()
	return f
}

func planValue(v any) string {
	switch v := v.(type) {
	case string:
		return v
	case []any:
		parts := make([]string, len(v))
		for i, item := range v {
			parts[i] = planValue(item)
		}
		return strings.Join(parts, ", ")
	}
	var b bytes.Buffer
	e := json.NewEncoder(&b)
	e.SetEscapeHTML(false)
	if err := e.Encode(v); err != nil {
		return fmt.Sprint(v)
	}
	return strings.TrimSpace(b.String())
}
