package gitops

import (
	"context"
	"reflect"
	"strings"
	"testing"
)

func TestCreatePR(t *testing.T) {
	const wantURL = "https://github.com/acme/widgets/pull/42"
	wantArgs := []string{"pr", "create",
		"--base", "main", "--head", "acy/t-1-do-the-thing",
		"--title", "Do the thing", "--body", "It does the thing.",
	}

	var gotDir, gotName string
	var gotArgs []string
	run := func(ctx context.Context, dir, name string, args ...string) (string, error) {
		gotDir, gotName, gotArgs = dir, name, args
		return wantURL + "\n", nil
	}

	url, err := CreatePR(context.Background(), run, "/work/wt", "main", "acy/t-1-do-the-thing", "Do the thing", "It does the thing.")
	if err != nil {
		t.Fatalf("CreatePR: %v", err)
	}
	if url != wantURL {
		t.Fatalf("url = %q, want %q", url, wantURL)
	}
	if gotDir != "/work/wt" {
		t.Fatalf("dir = %q, want /work/wt", gotDir)
	}
	if gotName != "gh" {
		t.Fatalf("name = %q, want gh", gotName)
	}
	if !reflect.DeepEqual(gotArgs, wantArgs) {
		t.Fatalf("args = %#v, want %#v", gotArgs, wantArgs)
	}
}

func TestCreatePREmptyTitle(t *testing.T) {
	run := func(ctx context.Context, dir, name string, args ...string) (string, error) {
		t.Fatal("gh should not run when the title is empty")
		return "", nil
	}
	if _, err := CreatePR(context.Background(), run, "/work/wt", "main", "acy/t-1", "   ", "body"); err == nil {
		t.Fatal("expected error for empty title, got nil")
	}
}

func TestCreatePRClipsBody(t *testing.T) {
	huge := strings.Repeat("a", maxPRBody+5000)
	var gotBody string
	run := func(ctx context.Context, dir, name string, args ...string) (string, error) {
		for i, a := range args {
			if a == "--body" {
				gotBody = args[i+1]
			}
		}
		return "https://github.com/acme/widgets/pull/1\n", nil
	}
	if _, err := CreatePR(context.Background(), run, "/work/wt", "main", "acy/t-1", "Title", huge); err != nil {
		t.Fatalf("CreatePR: %v", err)
	}
	if len(gotBody) != maxPRBody {
		t.Fatalf("clipped body length = %d, want %d", len(gotBody), maxPRBody)
	}
}
