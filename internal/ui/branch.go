package ui

import tea "charm.land/bubbletea/v2"

// branchMsg carries the resolved branch/SHA badge, "" when disabled or on
// error — never an error string, since a stale git failure is not something
// worth showing over whatever badge is already on screen.
type branchMsg struct{ branch string }

// resolveBranchCmd runs the resolver inside the tea.Cmd closure, never
// inline in Update, so the git call this ultimately makes stays off the
// event loop.
func resolveBranchCmd(resolve func() (string, error)) tea.Cmd {
	return func() tea.Msg {
		if resolve == nil {
			return branchMsg{}
		}
		branch, err := resolve()
		if err != nil {
			return branchMsg{}
		}
		return branchMsg{branch: branch}
	}
}
