package tickets

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/hweeks/always-click-yes/internal/gitops"
)

// failRunner is a gitops.Runner that is never expected to be called —
// most of these tests never touch Commit.
func failRunner(t *testing.T) gitops.Runner {
	t.Helper()
	return func(_ context.Context, _ string, name string, args ...string) (string, error) {
		t.Fatalf("unexpected command: %s %v", name, args)
		return "", nil
	}
}

func newTestStore(t *testing.T) *Store {
	t.Helper()
	s := New(t.TempDir(), "none", failRunner(t))
	s.now = func() time.Time { return time.Date(2026, 8, 5, 12, 0, 0, 0, time.UTC) }
	return s
}

func writeRaw(t *testing.T, s *Store, filename, content string) string {
	t.Helper()
	dir := s.dir()
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatalf("MkdirAll: %v", err)
	}
	path := filepath.Join(dir, filename)
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	return path
}

func TestPutGetListRoundTrip(t *testing.T) {
	s := newTestStore(t)

	base := Ticket{
		ID:     "base-ticket",
		Title:  "Base ticket",
		Status: StatusTodo,
		Body:   "The base ticket's brief.",
	}
	if err := s.Put(base); err != nil {
		t.Fatalf("Put(base): %v", err)
	}

	dependent := Ticket{
		ID:        "dependent-ticket",
		Title:     "Dependent ticket",
		Status:    StatusInProgress,
		Branch:    "acy/dependent-ticket",
		PR:        "https://github.com/example/repo/pull/1",
		DependsOn: []string{"base-ticket"},
		Body:      "The dependent ticket's brief.\n\nMore detail.",
	}
	if err := s.Put(dependent); err != nil {
		t.Fatalf("Put(dependent): %v", err)
	}

	got, err := s.Get("dependent-ticket")
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if got.ID != dependent.ID || got.Title != dependent.Title || got.Status != dependent.Status ||
		got.Branch != dependent.Branch || got.PR != dependent.PR {
		t.Fatalf("Get round-trip mismatch: got %+v, want fields of %+v", got, dependent)
	}
	if len(got.DependsOn) != 1 || got.DependsOn[0] != "base-ticket" {
		t.Fatalf("Get.DependsOn = %v, want [base-ticket]", got.DependsOn)
	}
	if !strings.HasPrefix(got.Body, "The dependent ticket's brief.") {
		t.Fatalf("Get.Body = %q, want it to start with the brief", got.Body)
	}
	if got.Updated.IsZero() {
		t.Fatal("Get.Updated is zero, want it stamped by Put")
	}

	list, err := s.List()
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(list) != 2 {
		t.Fatalf("List returned %d tickets, want 2", len(list))
	}
	if list[0].ID != "base-ticket" || list[1].ID != "dependent-ticket" {
		t.Fatalf("List not sorted by id: %v", []string{list[0].ID, list[1].ID})
	}
}

func TestGetNotFound(t *testing.T) {
	s := newTestStore(t)
	_, err := s.Get("nope")
	if !errors.Is(err, ErrNotFound) {
		t.Fatalf("Get(missing) error = %v, want ErrNotFound", err)
	}
}

func TestPutRejectsUnknownStatus(t *testing.T) {
	s := newTestStore(t)
	err := s.Put(Ticket{ID: "t1", Title: "T1", Status: "wontfix"})
	if err == nil {
		t.Fatal("Put with an unknown status: want error, got nil")
	}
}

func TestPutRejectsMissingTitle(t *testing.T) {
	s := newTestStore(t)
	err := s.Put(Ticket{ID: "t1", Title: "  ", Status: StatusTodo})
	if err == nil {
		t.Fatal("Put with a blank title: want error, got nil")
	}
}

func TestPutRejectsBadID(t *testing.T) {
	s := newTestStore(t)
	err := s.Put(Ticket{ID: "Not Valid!", Title: "T1", Status: StatusTodo})
	if err == nil {
		t.Fatal("Put with a bad id: want error, got nil")
	}
}

func TestPutRejectsDanglingDependsOn(t *testing.T) {
	s := newTestStore(t)
	err := s.Put(Ticket{ID: "t1", Title: "T1", Status: StatusTodo, DependsOn: []string{"nope"}})
	if err == nil {
		t.Fatal("Put depending on a ticket that doesn't exist: want error, got nil")
	}
}

func TestPutAllowsSelfDependsOn(t *testing.T) {
	// Put itself does not reject a self-reference — Validate is what catches
	// the resulting cycle. This documents that split of responsibility.
	s := newTestStore(t)
	if err := s.Put(Ticket{ID: "t1", Title: "T1", Status: StatusTodo, DependsOn: []string{"t1"}}); err != nil {
		t.Fatalf("Put with a self-referencing depends_on: %v", err)
	}
}

func TestPutGetRoundTripsStackOn(t *testing.T) {
	s := newTestStore(t)
	if err := s.Put(Ticket{ID: "base", Title: "Base", Status: StatusTodo}); err != nil {
		t.Fatalf("Put(base): %v", err)
	}
	if err := s.Put(Ticket{ID: "child", Title: "Child", Status: StatusTodo, StackOn: "base"}); err != nil {
		t.Fatalf("Put(child): %v", err)
	}

	got, err := s.Get("child")
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if got.StackOn != "base" {
		t.Fatalf("Get.StackOn = %q, want %q", got.StackOn, "base")
	}
}

func TestPutGetRoundTripsNoStackOn(t *testing.T) {
	s := newTestStore(t)
	if err := s.Put(Ticket{ID: "solo", Title: "Solo", Status: StatusTodo}); err != nil {
		t.Fatalf("Put: %v", err)
	}

	got, err := s.Get("solo")
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if got.StackOn != "" {
		t.Fatalf("Get.StackOn = %q, want empty", got.StackOn)
	}
}

func TestPutRejectsDanglingStackOn(t *testing.T) {
	s := newTestStore(t)
	err := s.Put(Ticket{ID: "t1", Title: "T1", Status: StatusTodo, StackOn: "nope"})
	if err == nil {
		t.Fatal("Put stacking on a ticket that doesn't exist: want error, got nil")
	}
	if !strings.Contains(err.Error(), "t1") || !strings.Contains(err.Error(), "nope") {
		t.Fatalf("error %q does not name both tickets", err)
	}
}

func TestPutRejectsSelfStackOn(t *testing.T) {
	// Unlike depends_on, stack_on rejects a self-reference immediately —
	// there is no legal single-ticket state it could settle into later.
	s := newTestStore(t)
	err := s.Put(Ticket{ID: "t1", Title: "T1", Status: StatusTodo, StackOn: "t1"})
	if err == nil {
		t.Fatal("Put with a self-referencing stack_on: want error, got nil")
	}
	if !strings.Contains(err.Error(), "t1") {
		t.Fatalf("error %q does not name the ticket", err)
	}
}

func TestPutReusesExistingFileAcrossTitleChange(t *testing.T) {
	s := newTestStore(t)
	if err := s.Put(Ticket{ID: "t1", Title: "Original title", Status: StatusTodo}); err != nil {
		t.Fatalf("Put(original): %v", err)
	}
	if err := s.Put(Ticket{ID: "t1", Title: "Renamed", Status: StatusTodo}); err != nil {
		t.Fatalf("Put(renamed): %v", err)
	}

	entries, err := os.ReadDir(s.dir())
	if err != nil {
		t.Fatalf("ReadDir: %v", err)
	}
	var mdFiles []string
	for _, e := range entries {
		if strings.HasSuffix(e.Name(), ".md") {
			mdFiles = append(mdFiles, e.Name())
		}
	}
	if len(mdFiles) != 1 {
		t.Fatalf("tickets dir has %v, want exactly one file for one ticket id", mdFiles)
	}

	got, err := s.Get("t1")
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if got.Title != "Renamed" {
		t.Fatalf("Get.Title = %q, want %q", got.Title, "Renamed")
	}
}

func TestPutAtomicLeavesNoTempDroppings(t *testing.T) {
	s := newTestStore(t)
	if err := s.Put(Ticket{ID: "t1", Title: "T1", Status: StatusTodo}); err != nil {
		t.Fatalf("Put: %v", err)
	}
	if err := s.Put(Ticket{ID: "t2", Title: "T2", Status: StatusTodo}); err != nil {
		t.Fatalf("Put: %v", err)
	}

	entries, err := os.ReadDir(s.dir())
	if err != nil {
		t.Fatalf("ReadDir: %v", err)
	}
	for _, e := range entries {
		if strings.HasPrefix(e.Name(), ".tmp-") {
			t.Fatalf("temp file left behind: %s", e.Name())
		}
	}
}

func TestListErrorsOnUnknownFrontmatterKey(t *testing.T) {
	s := newTestStore(t)
	path := writeRaw(t, s, "t1-bad.md", "---\n"+
		"id: t1\n"+
		"title: T1\n"+
		"status: todo\n"+
		"assignee: bob\n"+
		"updated: 2026-08-05T12:00:00Z\n"+
		"---\n\nBody.\n")

	_, err := s.List()
	if err == nil {
		t.Fatal("List with an unknown frontmatter key: want error, got nil")
	}
	if !strings.Contains(err.Error(), path) {
		t.Fatalf("error %q does not name the malformed file %q", err, path)
	}
}

func TestListErrorsOnBadStatus(t *testing.T) {
	s := newTestStore(t)
	path := writeRaw(t, s, "t1-bad.md", "---\n"+
		"id: t1\n"+
		"title: T1\n"+
		"status: sorta-done\n"+
		"updated: 2026-08-05T12:00:00Z\n"+
		"---\n\nBody.\n")

	_, err := s.List()
	if err == nil {
		t.Fatal("List with a bad status: want error, got nil")
	}
	if !strings.Contains(err.Error(), path) {
		t.Fatalf("error %q does not name the malformed file %q", err, path)
	}
}

func TestListErrorsOnMissingTitle(t *testing.T) {
	s := newTestStore(t)
	path := writeRaw(t, s, "t1-bad.md", "---\n"+
		"id: t1\n"+
		"status: todo\n"+
		"updated: 2026-08-05T12:00:00Z\n"+
		"---\n\nBody.\n")

	_, err := s.List()
	if err == nil {
		t.Fatal("List with no title field: want error, got nil")
	}
	if !strings.Contains(err.Error(), path) {
		t.Fatalf("error %q does not name the malformed file %q", err, path)
	}
}

func TestUpdateStatusAppendsToLogSection(t *testing.T) {
	s := newTestStore(t)
	if err := s.Put(Ticket{ID: "t1", Title: "T1", Status: StatusTodo, Body: "The brief."}); err != nil {
		t.Fatalf("Put: %v", err)
	}

	s.now = func() time.Time { return time.Date(2026, 8, 5, 12, 30, 0, 0, time.UTC) }
	if err := s.UpdateStatus("t1", StatusInProgress, "started work"); err != nil {
		t.Fatalf("UpdateStatus: %v", err)
	}

	s.now = func() time.Time { return time.Date(2026, 8, 5, 13, 45, 0, 0, time.UTC) }
	if err := s.UpdateStatus("t1", StatusInReview, "opened the PR"); err != nil {
		t.Fatalf("UpdateStatus: %v", err)
	}

	got, err := s.Get("t1")
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if got.Status != StatusInReview {
		t.Fatalf("Status = %q, want %q", got.Status, StatusInReview)
	}
	if strings.Count(got.Body, logHeading) != 1 {
		t.Fatalf("Body has %d \"## Log\" headings, want exactly 1:\n%s", strings.Count(got.Body, logHeading), got.Body)
	}
	if !strings.Contains(got.Body, "2026-08-05T12:30:00Z: started work") {
		t.Fatalf("Body missing first log entry:\n%s", got.Body)
	}
	if !strings.Contains(got.Body, "2026-08-05T13:45:00Z: opened the PR") {
		t.Fatalf("Body missing second log entry:\n%s", got.Body)
	}
	firstIdx := strings.Index(got.Body, "started work")
	secondIdx := strings.Index(got.Body, "opened the PR")
	if firstIdx == -1 || secondIdx == -1 || firstIdx > secondIdx {
		t.Fatalf("log entries not in chronological order:\n%s", got.Body)
	}
	if !strings.HasPrefix(got.Body, "The brief.") {
		t.Fatalf("Body lost its original brief:\n%s", got.Body)
	}
}

func TestUpdateFieldsSetsBranchAndPR(t *testing.T) {
	s := newTestStore(t)
	if err := s.Put(Ticket{ID: "t1", Title: "T1", Status: StatusTodo}); err != nil {
		t.Fatalf("Put: %v", err)
	}
	if err := s.UpdateFields("t1", StatusInProgress, "", "agent/t1", ""); err != nil {
		t.Fatalf("UpdateFields: %v", err)
	}

	got, err := s.Get("t1")
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if got.Status != StatusInProgress || got.Branch != "agent/t1" || got.PR != "" {
		t.Fatalf("Get = %+v, want status in-progress, branch agent/t1, no pr", got)
	}
}

// A later call that omits branch/pr must not clobber what an earlier call
// already recorded — the model only sends what changed at each transition.
func TestUpdateFieldsPreservesBranchAndPROnLaterUpdate(t *testing.T) {
	s := newTestStore(t)
	if err := s.Put(Ticket{ID: "t1", Title: "T1", Status: StatusTodo}); err != nil {
		t.Fatalf("Put: %v", err)
	}
	if err := s.UpdateFields("t1", StatusInProgress, "", "agent/t1", ""); err != nil {
		t.Fatalf("UpdateFields(branch): %v", err)
	}
	if err := s.UpdateFields("t1", StatusInReview, "", "", "https://example.com/pr/1"); err != nil {
		t.Fatalf("UpdateFields(pr): %v", err)
	}

	got, err := s.Get("t1")
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if got.Status != StatusInReview {
		t.Fatalf("Status = %q, want %q", got.Status, StatusInReview)
	}
	if got.Branch != "agent/t1" {
		t.Fatalf("Branch = %q, want it preserved from the earlier update", got.Branch)
	}
	if got.PR != "https://example.com/pr/1" {
		t.Fatalf("PR = %q, want it set by this update", got.PR)
	}

	if err := s.UpdateFields("t1", StatusMerged, "", "", ""); err != nil {
		t.Fatalf("UpdateFields(merged): %v", err)
	}
	got, err = s.Get("t1")
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if got.Branch != "agent/t1" || got.PR != "https://example.com/pr/1" {
		t.Fatalf("Get = %+v, want branch and pr both preserved through a status-only update", got)
	}
}

func TestUpdateStatusWithoutNoteLeavesBodyUnchanged(t *testing.T) {
	s := newTestStore(t)
	if err := s.Put(Ticket{ID: "t1", Title: "T1", Status: StatusTodo, Body: "The brief.\n"}); err != nil {
		t.Fatalf("Put: %v", err)
	}
	if err := s.UpdateStatus("t1", StatusInProgress, ""); err != nil {
		t.Fatalf("UpdateStatus: %v", err)
	}
	got, err := s.Get("t1")
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if got.Body != "The brief.\n" {
		t.Fatalf("Body = %q, want unchanged", got.Body)
	}
	if strings.Contains(got.Body, logHeading) {
		t.Fatalf("Body gained a Log section with no note:\n%s", got.Body)
	}
}
