package ui

import (
	"strconv"
	"strings"
	"testing"

	"github.com/charmbracelet/lipgloss"
)

func TestClampBlockTruncates(t *testing.T) {
	var lines []string
	for i := range 20 {
		lines = append(lines, "line"+strconv.Itoa(i))
	}
	body := strings.Join(lines, "\n")
	out := clampBlock(body, 80, 5, colGood, lipgloss.NewStyle().Foreground(colDim))

	if !strings.Contains(out, "+15 more lines") {
		t.Errorf("expected a '+15 more lines' footer, got:\n%s", out)
	}
	if !strings.Contains(out, "line4") {
		t.Error("expected kept line 'line4' in output")
	}
	if strings.Contains(out, "line5") {
		t.Error("did not expect truncated 'line5' in output")
	}
}

func TestClampBlockEmpty(t *testing.T) {
	if out := clampBlock("   \n  ", 80, 10, colDim, lipgloss.NewStyle()); out != "" {
		t.Errorf("expected empty output for blank body, got %q", out)
	}
}

func TestClampBlockSingularFooter(t *testing.T) {
	out := clampBlock("a\nb\nc", 80, 2, colDim, lipgloss.NewStyle())
	if !strings.Contains(out, "+1 more line") || strings.Contains(out, "more lines") {
		t.Errorf("expected singular '+1 more line', got:\n%s", out)
	}
}
