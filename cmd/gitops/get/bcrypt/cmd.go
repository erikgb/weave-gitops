package bcrypt

import (
	"fmt"
	"io"
	"os"

	"github.com/spf13/cobra"
	"golang.org/x/crypto/bcrypt"
	"golang.org/x/term"

	"github.com/weaveworks/weave-gitops/cmd/gitops/config"
)

func HashCommand(opts *config.Options) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "bcrypt-hash",
		Short: "Generates a hashed secret",
		Example: `
PASSWORD="<your password>"
echo -n $PASSWORD | gitops get bcrypt-hash
`,
		SilenceUsage:  true,
		SilenceErrors: true,
		RunE:          hashCommandRunE(),
	}

	return cmd
}

func hashCommandRunE() func(*cobra.Command, []string) error {
	return func(cmd *cobra.Command, args []string) error {
		file := os.Stdin
		stats, err := file.Stat()

		if err != nil {
			return err
		}

		var p []byte

		if (stats.Mode() & os.ModeCharDevice) == 0 {
			p, err = io.ReadAll(os.Stdin)

			if err != nil {
				return err
			}
		} else {
			fmt.Print("Enter Password: ")

			p, err = term.ReadPassword(int(os.Stdin.Fd()))

			fmt.Println()

			if err != nil {
				return err
			}
		}

		secret, err := bcrypt.GenerateFromPassword(p, bcrypt.DefaultCost)

		if err != nil {
			return err
		}

		fmt.Println(string(secret))

		return nil
	}
}
