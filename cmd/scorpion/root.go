// Package scorpion provides the CLI commands for the Scorpion SSE server.
package scorpion

import (
	"fmt"
	"os"

	"github.com/spf13/cobra"
)

var rootCmd = &cobra.Command{
	Use:   "scorpion",
	Short: "Scorpion — high-performance SSE server",
	Long:  `Scorpion is a lightweight SSE server using pre-auth JWT tickets backed by Redis.`,
}

// Execute runs the root CLI command.
func Execute() {
	if err := rootCmd.Execute(); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}
