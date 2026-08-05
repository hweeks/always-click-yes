package cli

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"sync"

	"github.com/spf13/cobra"

	"github.com/hweeks/always-click-yes/internal/config"
	"github.com/hweeks/always-click-yes/internal/fleet"
)

// newFleetCmd is the visible "fleet" command group: unlike hook/engineer,
// this is something a human runs directly to check on their own .acy.json
// before trusting arch mode to it.
func newFleetCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "fleet",
		Short: "Manage the hosts arch mode's engineers run on",
	}
	cmd.AddCommand(newFleetDoctorCmd())
	return cmd
}

// hostReport is one host's Doctor results — the unit `--json` prints one of
// per configured host.
type hostReport struct {
	Host   string        `json:"host"`
	Checks []fleet.Check `json:"checks"`
}

func newFleetDoctorCmd() *cobra.Command {
	var asJSON bool
	cmd := &cobra.Command{
		Use:   "doctor",
		Short: "Check every fleet host's ssh, acy, claude, gh, git and state-dir health",
		RunE: func(cmd *cobra.Command, args []string) error {
			return runFleetDoctor(cmd.Context(), cmd.OutOrStdout(), asJSON)
		},
	}
	cmd.Flags().BoolVar(&asJSON, "json", false, "print machine-readable results instead of a table")
	return cmd
}

// runFleetDoctor loads the project's .acy.json, runs fleet.Doctor against
// every configured host, prints the results, and reports a nonzero exit if
// any check anywhere failed.
func runFleetDoctor(ctx context.Context, out io.Writer, asJSON bool) error {
	cwd, err := os.Getwd()
	if err != nil {
		return err
	}
	file, found, err := config.LoadFile(cwd)
	if err != nil {
		return err
	}
	if !found || file.Fleet == nil {
		return fmt.Errorf("acy fleet doctor: %s has no \"fleet\" section", config.FileName)
	}

	reports := doctorAllHosts(ctx, file.Fleet.Hosts, file.Fleet.BaseBranch)

	if asJSON {
		enc := json.NewEncoder(out)
		enc.SetIndent("", "  ")
		if err := enc.Encode(reports); err != nil {
			return fmt.Errorf("acy fleet doctor: encoding results: %w", err)
		}
	} else {
		printDoctorTable(out, reports)
	}

	for _, r := range reports {
		for _, c := range r.Checks {
			if !c.OK {
				return fmt.Errorf("acy fleet doctor: one or more checks failed")
			}
		}
	}
	return nil
}

// doctorAllHosts runs fleet.Doctor for every host concurrently — nothing
// about one host's checks depends on another's — while each host's own
// checks still run sequentially, inside fleet.Doctor, in its fixed order.
func doctorAllHosts(ctx context.Context, hosts []config.FleetHost, base string) []hostReport {
	reports := make([]hostReport, len(hosts))
	var wg sync.WaitGroup
	for i, h := range hosts {
		wg.Add(1)
		go func(i int, h config.FleetHost) {
			defer wg.Done()
			reports[i] = hostReport{Host: h.Name, Checks: fleet.Doctor(ctx, h, base)}
		}(i, h)
	}
	wg.Wait()
	return reports
}

// printDoctorTable renders one block per host, one line per check: a ✓/✗
// mark, the check's name, and its detail when it has one.
func printDoctorTable(out io.Writer, reports []hostReport) {
	for _, r := range reports {
		_, _ = fmt.Fprintf(out, "%s\n", r.Host)
		for _, c := range r.Checks {
			mark := "✗"
			if c.OK {
				mark = "✓"
			}
			if c.Detail != "" {
				_, _ = fmt.Fprintf(out, "  %s %-8s %s\n", mark, c.Name, c.Detail)
			} else {
				_, _ = fmt.Fprintf(out, "  %s %-8s\n", mark, c.Name)
			}
		}
	}
}
