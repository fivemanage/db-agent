package cmd

import (
	"encoding/json"
	"fmt"
	"runtime"

	"github.com/spf13/cobra"
)

var versionJSON bool

var versionCmd = &cobra.Command{
	Use:   "version",
	Short: "Print the agent version",
	Args:  cobra.NoArgs,
	RunE: func(cmd *cobra.Command, args []string) error {
		if versionJSON {
			return json.NewEncoder(cmd.OutOrStdout()).Encode(struct {
				Version   string `json:"version"`
				PublicKey string `json:"public_key"`
				APIURL    string `json:"api_url"`
				GoVersion string `json:"go_version"`
				OS        string `json:"os"`
				Arch      string `json:"arch"`
			}{version, updatePublicKey, defaultAPIURL, runtime.Version(), runtime.GOOS, runtime.GOARCH})
		}
		fmt.Fprintln(cmd.OutOrStdout(), version)
		return nil
	},
}

func init() {
	versionCmd.Flags().BoolVar(&versionJSON, "json", false, "print public build and update trust metadata as JSON")
	rootCmd.AddCommand(versionCmd)
}
