package metrics

import (
	"strings"
	"testing"
)

func TestRegistry(t *testing.T) {
	var r Registry
	c := r.NewCounter("squared_tasks_total", "Tasks by result.", "kind", "result")
	s := r.NewSummary("squared_task_seconds", "Task duration.", "kind")
	c.Inc("tenant.sync", "done")
	c.Inc("tenant.sync", "done")
	c.Inc("deployment.sync", `bad "quote"`)
	s.Observe(0.5, "tenant.sync")
	s.Observe(1.25, "tenant.sync")

	var b strings.Builder
	r.Write(&b)
	want := `# HELP squared_tasks_total Tasks by result.
# TYPE squared_tasks_total counter
squared_tasks_total{kind="deployment.sync",result="bad \"quote\""} 1
squared_tasks_total{kind="tenant.sync",result="done"} 2
# HELP squared_task_seconds Task duration.
# TYPE squared_task_seconds summary
squared_task_seconds_sum{kind="tenant.sync"} 1.75
squared_task_seconds_count{kind="tenant.sync"} 2
`
	if b.String() != want {
		t.Fatalf("got:\n%s\nwant:\n%s", b.String(), want)
	}
}
