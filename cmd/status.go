package cmd

import (
	"fmt"
	"io"
	"path/filepath"
	"time"

	"github.com/fivemanage/db-agent/internal/backup"
	"github.com/fivemanage/db-agent/internal/config"
	"github.com/spf13/cobra"
)

func printBackupStatus(path string, output io.Writer) error {
	dir := filepath.Join(filepath.Dir(path), "backup-transfer")
	snapshot, err := backup.ReadSnapshot(dir)
	if err != nil {
		return err
	}
	schedule, err := backup.ReadSchedule(dir)
	if err != nil {
		return err
	}
	if snapshot.Latest == nil {
		fmt.Fprintln(output, "Last backup: no recorded run.")
	} else {
		run := snapshot.Latest
		fmt.Fprintf(output, "Last backup: %s (started %s, pending: %t).\n", run.Status, run.StartedAt.Local().Format(time.RFC3339), run.Pending)
	}
	fmt.Fprintf(output, "Recorded backup activity: %s.\n", snapshot.Activity)
	if schedule == nil || schedule.NextRun == nil {
		fmt.Fprintln(output, "Next scheduled backup: not recorded; start the agent service.")
	} else {
		fmt.Fprintf(output, "Next scheduled backup (last recorded): %s.\n", schedule.NextRun.Local().Format(time.RFC3339))
	}
	fmt.Fprintln(output, "Recorded backup state is not proof of service health or a live schedule.")
	return nil
}

func init() {
	rootCmd.AddCommand(&cobra.Command{
		Use: "status", Short: "Show recorded backup status and the next scheduled run", Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			return printBackupStatus(config.ResolvePath(configPath), cmd.OutOrStdout())
		},
	})
}
