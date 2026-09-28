package cmd

import "github.com/spf13/cobra"

var version = "dev"
var configPath string

var rootCmd = &cobra.Command{
	Use:           "db-agent",
	Short:         "Backup and metrics Agent for MySQL and MariaDB databases",
	SilenceErrors: true,
	SilenceUsage:  true,
	Version:       version,
}

func Execute() error {
	return rootCmd.Execute()
}

func init() {
	rootCmd.PersistentFlags().StringVar(&configPath, "config", "", "config file path (default: platform path or DB_AGENT_CONFIG)")
}
