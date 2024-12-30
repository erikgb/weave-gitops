package dashboard

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"os"
	"regexp"
	"time"

	"github.com/spf13/cobra"
	"k8s.io/cli-runtime/pkg/genericclioptions"

	"github.com/weaveworks/weave-gitops/cmd/gitops/cmderrors"
	"github.com/weaveworks/weave-gitops/cmd/gitops/config"
	"github.com/weaveworks/weave-gitops/pkg/kube"
	"github.com/weaveworks/weave-gitops/pkg/logger"
	"github.com/weaveworks/weave-gitops/pkg/run"
	"github.com/weaveworks/weave-gitops/pkg/run/install"
)

const (
	defaultAdminUsername = "admin"
)

type DashboardCommandFlags struct {
	// Create command flags.
	Export      bool
	Timeout     time.Duration
	ValuesFiles []string
	// Overridden global flags.
	Username string
	Password string
	// Global flags.
	Namespace  string
	KubeConfig string
	// Flags, created by genericclioptions.
	Context string
}

var flags DashboardCommandFlags

var kubeConfigArgs *genericclioptions.ConfigFlags

func DashboardCommand(opts *config.Options) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "dashboard",
		Short: "Create a HelmRepository and HelmRelease to deploy Weave GitOps",
		Long:  "Create a HelmRepository and HelmRelease to deploy Weave GitOps",
		Example: `
# Create a HelmRepository and HelmRelease to deploy Weave GitOps
gitops create dashboard ww-gitops \
  --password=$PASSWORD \
  --export > ./clusters/my-cluster/weave-gitops-dashboard.yaml
		`,
		SilenceUsage:      true,
		SilenceErrors:     true,
		PreRunE:           createDashboardCommandPreRunE(&opts.Endpoint),
		RunE:              createDashboardCommandRunE(opts),
		DisableAutoGenTag: true,
	}

	cmdFlags := cmd.Flags()

	cmdFlags.StringVar(&flags.Username, "username", "admin", "The username of the dashboard admin user.")
	cmdFlags.StringVar(&flags.Password, "password", "", "The password of the dashboard admin user.")
	cmdFlags.StringSliceVar(&flags.ValuesFiles, "values", nil, "Local path to values.yaml files for HelmRelease, also accepts comma-separated values.")

	kubeConfigArgs = run.GetKubeConfigArgs()

	kubeConfigArgs.AddFlags(cmd.Flags())

	return cmd
}

func createDashboardCommandPreRunE(endpoint *string) func(*cobra.Command, []string) error {
	return func(cmd *cobra.Command, args []string) error {
		numArgs := len(args)

		if numArgs == 0 {
			return cmderrors.ErrNoName
		}

		if numArgs > 1 {
			return cmderrors.ErrMultipleNames
		}

		name := args[0]
		if !validateObjectName(name) {
			return fmt.Errorf("name '%s' is invalid, it should adhere to standard defined in RFC 1123, the name can only contain alphanumeric characters or '-'", name)
		}

		return nil
	}
}

func createDashboardCommandRunE(opts *config.Options) func(*cobra.Command, []string) error {
	return func(cmd *cobra.Command, args []string) error {
		var err error

		if flags.Namespace, err = cmd.Flags().GetString("namespace"); err != nil {
			return err
		}

		kubeConfigArgs.Namespace = &flags.Namespace

		if flags.KubeConfig, err = cmd.Flags().GetString("kubeconfig"); err != nil {
			return err
		}

		if flags.Context, err = cmd.Flags().GetString("context"); err != nil {
			return err
		}

		if flags.Export, err = cmd.Flags().GetBool("export"); err != nil {
			return err
		}

		if flags.Timeout, err = cmd.Flags().GetDuration("timeout"); err != nil {
			return err
		}

		if flags.ValuesFiles, err = cmd.Flags().GetStringSlice("values"); err != nil {
			return err
		}

		var output io.Writer

		if flags.Export {
			output = &bytes.Buffer{}
		} else {
			output = os.Stdout
		}

		log := logger.NewCLILogger(output)

		log.Generatef("Generating GitOps Dashboard manifests ...")

		var passwordHash string

		if flags.Password != "" {
			passwordHash, err = install.GeneratePasswordHash(log, flags.Password)
			if err != nil {
				return err
			}
		}

		dashboardName := args[0]

		adminUsername := flags.Username

		if adminUsername == "" && flags.Password != "" {
			adminUsername = defaultAdminUsername
		}

		dashboardObjects, err := install.CreateDashboardObjects(log, dashboardName, flags.Namespace, adminUsername, passwordHash, "", "", flags.ValuesFiles)
		if err != nil {
			return fmt.Errorf("error creating dashboard objects: %w", err)
		}

		log.Successf("Generated GitOps Dashboard manifests")

		if flags.Export {
			fmt.Println("---")
			fmt.Println(string(dashboardObjects.Manifests))

			return nil
		}

		if flags.KubeConfig != "" {
			kubeConfigArgs.KubeConfig = &flags.KubeConfig

			if flags.Context == "" {
				log.Failuref("A context should be provided if a kubeconfig is provided")
				return cmderrors.ErrNoContextForKubeConfig
			}
		}

		log.Actionf("Checking for a cluster in the kube config ...")

		var contextName string

		if flags.Context != "" {
			contextName = flags.Context
		} else {
			_, contextName, err = kube.RestConfig()
			if err != nil {
				log.Failuref("Error getting a restconfig: %v", err.Error())
				return cmderrors.ErrNoCluster
			}
		}

		cfg, err := kubeConfigArgs.ToRESTConfig()
		if err != nil {
			return fmt.Errorf("error getting a restconfig from kube config args: %w", err)
		}

		kubeClientOpts := run.GetKubeClientOptions()
		kubeClientOpts.BindFlags(cmd.Flags())

		kubeClient, err := run.GetKubeClient(log, contextName, cfg, kubeClientOpts)
		if err != nil {
			return cmderrors.ErrGetKubeClient
		}

		ctx, cancel := context.WithTimeout(context.Background(), flags.Timeout)
		defer cancel()

		log.Actionf("Checking if Flux is already installed ...")

		if fluxVersion, guessed, err := install.GetFluxVersion(ctx, log, kubeClient); err != nil {
			log.Failuref("Flux is not found")
			return err
		} else {
			if guessed {
				log.Warningf("Flux version could not be determined, assuming %s by mapping from the version of the Source controller", fluxVersion)
			} else {
				log.Successf("Flux %s is already installed on the %s namespace.", fluxVersion.FluxVersion, fluxVersion.FluxNamespace)
			}
		}

		dashboardType, _, err := install.GetInstalledDashboard(ctx, kubeClient, flags.Namespace, map[install.DashboardType]bool{
			install.DashboardTypeOSS: true, install.DashboardTypeEnterprise: true,
		})
		if err != nil {
			log.Failuref("Error getting installed dashboard")
			return err
		}

		switch dashboardType {
		case install.DashboardTypeEnterprise:
			log.Warningf("GitOps Enterprise Dashboard was found. GitOps OSS Dashboard will not be installed")
			return nil
		case install.DashboardTypeOSS:
			log.Warningf("GitOps Dashboard was found. GitOps Dashboard will not be installed")
			return nil
		}

		log.Actionf("Applying GitOps Dashboard manifests")
		err = install.InstallDashboard(ctx, log, kubeClient, dashboardObjects)
		if err != nil {
			return fmt.Errorf("gitops dashboard installation failed: %w", err)
		} else {
			log.Successf("GitOps Dashboard has been installed")
		}

		log.Actionf("Request reconciliation of dashboard (timeout %v) ...", flags.Timeout)

		log.Waitingf("Waiting for GitOps Dashboard reconciliation")

		if err := install.ReconcileDashboard(ctx, kubeClient, dashboardName, flags.Namespace, "", flags.Timeout); err != nil {
			log.Failuref("Error requesting reconciliation of dashboard: %v", err.Error())
		} else {
			log.Successf("GitOps Dashboard %s is ready", dashboardName)
		}

		log.Successf("Installed GitOps Dashboard")

		return nil
	}
}

func validateObjectName(name string) bool {
	r := regexp.MustCompile(`^[a-z0-9]([a-z0-9\\-]){0,61}[a-z0-9]$`)
	return r.MatchString(name)
}
