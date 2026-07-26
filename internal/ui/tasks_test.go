package ui

import (
	"strings"
	"testing"
	"time"

	"github.com/hweeks/always-click-yes/internal/state"
)

func finishedTask(id, title, outcome string, cost float64, cacheRead int64) state.Task {
	start := time.Date(2026, 7, 26, 12, 0, 0, 0, time.UTC)
	return state.Task{
		ID: id, Title: title, Outcome: outcome, CostUSD: cost,
		Tokens:    state.Tokens{CacheRead: cacheRead},
		StartedAt: start, EndedAt: start.Add(time.Minute),
	}
}

// A run that has delegated nothing must say so in words. The alternative — an
// empty table under a header, or a total of $0.0000 — reads like a ledger that
// lost its rows rather than one that never had any.
func TestTaskReportSaysNothingWasDispatchedYet(t *testing.T) {
	m := New(nil, Config{})
	got := m.taskReport()
	if got != "no tasks dispatched yet" {
		t.Errorf("taskReport() = %q, want the empty-ledger line", got)
	}
	if strings.Contains(got, "$") {
		t.Errorf("taskReport() showed a cost with no tasks: %q", got)
	}
}

// A task with no end time is still running. Its blank outcome and zero cost are
// "not in yet", not "finished badly", and the row has to say which.
func TestRunningTaskReadsAsRunning(t *testing.T) {
	m := New(nil, Config{})
	m.tasks = []state.Task{{
		ID: "t1", Title: "port the parser", StartedAt: time.Now(),
	}}

	got := m.taskReport()
	if !strings.Contains(got, "running") {
		t.Errorf("taskReport() should mark an unfinished task as running:\n%s", got)
	}
}

// The mirror case: a task that *did* end without an outcome gets a dash, so the
// column is never blank and can never be mistaken for the running case above.
func TestFinishedTaskWithoutAnOutcomeShowsADash(t *testing.T) {
	m := New(nil, Config{})
	m.tasks = []state.Task{finishedTask("t1", "port the parser", "", 0.5, 1000)}

	got := m.taskReport()
	if !strings.Contains(got, "—") {
		t.Errorf("taskReport() should dash an empty outcome:\n%s", got)
	}
	if strings.Contains(got, "running") {
		t.Errorf("taskReport() called a finished task running:\n%s", got)
	}
}

// The row is the only surviving account of a child that has already scrolled
// away, so it has to carry the id, what was asked, how it ended and what it cost.
func TestFinishedTaskShowsItsOutcomeAndCost(t *testing.T) {
	m := New(nil, Config{})
	m.tasks = []state.Task{finishedTask("t3", "rewrite the gate", "completed", 0.1234, 812_000)}

	got := m.taskReport()
	for _, want := range []string{"t3", "rewrite the gate", "completed", "$0.1234", "812k"} {
		if !strings.Contains(got, want) {
			t.Errorf("taskReport() missing %q:\n%s", want, got)
		}
	}
}

// Cost is per task and cache reads are per task; the number a human actually
// wants is the sum, and adding four-decimal dollars by eye is not the job.
func TestTaskReportTotalsSumCostAndCacheReads(t *testing.T) {
	m := New(nil, Config{})
	m.tasks = []state.Task{
		finishedTask("t1", "one", "completed", 1.50, 40_000),
		finishedTask("t2", "two", "blocked", 2.25, 60_000),
	}

	got := m.taskReport()
	if !strings.Contains(got, "$3.7500") {
		t.Errorf("taskReport() total cost should be $3.7500:\n%s", got)
	}
	if !strings.Contains(got, "100k") {
		t.Errorf("taskReport() total cache reads should be 100k:\n%s", got)
	}
	if !strings.Contains(got, "2 task(s)") {
		t.Errorf("taskReport() total should count 2 tasks:\n%s", got)
	}
}

// state.TrimTasks drops the oldest rows but the dispatch count keeps climbing,
// so on a long run the total is a total of what is *shown*. Saying so is the
// difference between an elided ledger and a wrong one.
func TestTaskReportFlagsATrimmedLedger(t *testing.T) {
	m := New(nil, Config{})
	m.tasks = []state.Task{finishedTask("t141", "the newest one", "completed", 1, 1000)}
	m.dispatches = 141

	got := m.taskReport()
	if !strings.Contains(got, "141 task(s) dispatched") {
		t.Errorf("taskReport() should report the full dispatch count:\n%s", got)
	}
	if !strings.Contains(got, "dropped") {
		t.Errorf("taskReport() should say older rows were dropped:\n%s", got)
	}

	// With nothing elided the note must stay away — it would otherwise imply a
	// loss on every ordinary run.
	m.dispatches = 1
	if got := m.taskReport(); strings.Contains(got, "dropped") {
		t.Errorf("taskReport() warned of a trim that did not happen:\n%s", got)
	}
}

// The report is only reachable through the command, and it lands in the
// transcript as a meta entry rather than being sent to claude.
func TestTasksCommandAppendsTheReport(t *testing.T) {
	m := New(nil, Config{})
	m.tasks = []state.Task{finishedTask("t1", "rewrite the gate", "completed", 0.25, 5000)}
	before := len(m.entries)

	if cmd := m.runCommand("tasks", ""); cmd != nil {
		t.Error("runCommand(tasks) should not return a tea.Cmd")
	}
	if len(m.entries) != before+1 {
		t.Fatalf("entries = %d, want %d (one report appended)", len(m.entries), before+1)
	}
	e := m.entries[len(m.entries)-1]
	if e.kind != eMeta {
		t.Errorf("entry kind = %v, want eMeta", e.kind)
	}
	if !strings.Contains(e.body, "rewrite the gate") {
		t.Errorf("entry body is not the task report:\n%s", e.body)
	}
}
