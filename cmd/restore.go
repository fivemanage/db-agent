package cmd

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os/signal"
	"path/filepath"
	"runtime"
	"strings"
	"syscall"

	"github.com/fivemanage/db-agent/internal/config"
	"github.com/fivemanage/db-agent/internal/controlplane"
	"github.com/fivemanage/db-agent/internal/key"
	"github.com/fivemanage/db-agent/internal/restore"
	"github.com/spf13/cobra"
)

func newRestoreCommand() *cobra.Command {
	var request restore.Request
	var options restore.Options
	var keyFile string
	command := &cobra.Command{
		Use:   "restore --backup-id <id> | --file <encrypted-backup>",
		Short: "Recover verified SQL using the existing local key automatically",
		Long:  "Download or open an encrypted backup and recover a private SQL file. The configured local key is used automatically; no database is connected to or changed. --key-file is only for offline recovery from --file on a replacement machine.",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			if (request.BackupID == "") == (request.File == "") {
				return errors.New("choose exactly one --backup-id or --file")
			}
			if keyFile != "" && request.File == "" {
				return errors.New("normal restore uses the existing local key automatically; --key-file is only for offline recovery with --file")
			}
			if options.Timeout <= 0 || options.MaxOutputBytes <= 0 {
				return errors.New("restore timeout and maximum output bytes must be positive")
			}
			ctx, stop := signal.NotifyContext(cmd.Context(), syscall.SIGINT, syscall.SIGTERM)
			defer stop()
			return runConfiguredRestore(ctx, config.ResolvePath(configPath), request, keyFile, options, cmd.OutOrStdout())
		},
	}
	command.Flags().StringVar(&request.BackupID, "backup-id", "", "backup to download using this agent's access")
	command.Flags().StringVar(&request.File, "file", "", "already-downloaded encrypted backup (no API access needed)")
	command.Flags().StringVar(&request.Output, "output", "restore.sql", "new private SQL output file (never overwritten)")
	command.Flags().StringVar(&keyFile, "key-file", "", "private saved key file for offline recovery only; never put key contents in a command")
	command.Flags().DurationVar(&options.Timeout, "timeout", restore.DefaultTimeout, "maximum complete restore duration")
	command.Flags().Int64Var(&options.MaxOutputBytes, "max-output-bytes", restore.DefaultMaxOutputBytes, "maximum decompressed SQL bytes; keeps a separate 256 MiB free-space reserve")
	return command
}

func runConfiguredRestore(ctx context.Context, path string, request restore.Request, keyFile string, options restore.Options, output io.Writer) error {
	var lookup restore.Lookup
	if keyFile != "" {
		if request.File == "" || request.BackupID != "" {
			return errors.New("a saved key file can only be used with an offline encrypted file")
		}
		k, err := key.Load(keyFile)
		if err != nil {
			return errors.New("cannot read the saved private recovery key; check its format, permissions, and local account access")
		}
		request.Key = k
	} else {
		settings, err := config.LoadDiagnostic(path)
		if err != nil {
			return err
		}
		request.Key, err = restore.ConfiguredKey(path, settings.Encryption)
		if err != nil {
			return err
		}
		if request.BackupID != "" {
			lookup, err = controlplane.New(settings.APIURL, settings.AgentCredential, version, nil)
			if err != nil {
				return err
			}
		}
	}
	runner, err := restore.New(lookup, options)
	if err != nil {
		return err
	}
	result, err := runner.Restore(ctx, request)
	if err != nil {
		return err
	}
	fmt.Fprintf(output, "Verified SQL saved to %q (%d bytes). No database was changed.\n", request.Output, result.OutputBytes)
	printRestoreImport(output, request.Output, runtime.GOOS)
	return nil
}

func printRestoreImport(output io.Writer, path, goos string) {
	fmt.Fprintln(output, "The SQL contains the ORIGINAL database names (CREATE DATABASE/USE). Choosing a different default database does not redirect it. Import only into an isolated restore server, not production.")
	fmt.Fprintln(output, "For manual import, replace RESTORE_HOST, RESTORE_PORT, and RESTORE_USER with that isolated server's settings:")
	if strings.ContainsAny(path, "\r\n\x00") {
		fmt.Fprintln(output, "Use your MySQL/MariaDB client's SQL-file import; this filename cannot be shown safely as a shell command.")
		return
	}
	path, err := filepath.Abs(path)
	if err != nil {
		return
	}
	if goos == "windows" {
		if strings.ContainsAny(path, "\"%!&|^<>") {
			fmt.Fprintln(output, "Use your MySQL/MariaDB client's SQL-file import; this filename requires manual shell escaping.")
			return
		}
		fmt.Fprintln(output, "Run in Command Prompt (cmd.exe), not PowerShell:")
		fmt.Fprintf(output, "mysql --host=RESTORE_HOST --port=RESTORE_PORT --user=RESTORE_USER --password < \"%s\"\n", path)
		return
	}
	quoted := "'" + strings.ReplaceAll(path, "'", "'\"'\"'") + "'"
	fmt.Fprintf(output, "mysql --host=RESTORE_HOST --port=RESTORE_PORT --user=RESTORE_USER --password < %s\n", quoted)
}

func init() { rootCmd.AddCommand(newRestoreCommand()) }
