package cmd

import (
	"context"
	"errors"
	"fmt"
	"io"
	"strconv"
	"strings"

	"github.com/olekukonko/tablewriter"
	"github.com/spf13/cobra"

	"phenix/api/vlan"
	"phenix/util"
	"phenix/util/plog"
	"phenix/util/printer"
)

// cliView sees everything: the CLI acts with the store's and minimega's access.
func cliView() vlan.View {
	return vlan.View{Visible: func(string) bool { return true }, All: true}
}

// errBadVLANID is returned for a VLAN ID argument that is not one.
var errBadVLANID = errors.New("not a VLAN ID")

// parseCLIVLANID parses a VLAN ID argument.
func parseCLIVLANID(arg string) (int, error) {
	id, err := strconv.Atoi(arg)
	if err != nil || !vlan.ValidID(id) {
		return 0, fmt.Errorf("%w: %q is not a whole number in %d–%d", errBadVLANID, arg, vlan.MinID, vlan.MaxID)
	}

	return id, nil
}

// optionalID prints a source's ID, or "-" for none.
func optionalID(id *int) string {
	if id == nil {
		return "-"
	}

	return strconv.Itoa(*id)
}

// printVLANReport writes the rows and issues of a merged VLAN report to w.
func printVLANReport(w io.Writer, rep vlan.Report) {
	if !rep.Minimega.OK {
		fmt.Fprintf(w, "minimega did not answer (%s): the table shows the phēnix side only.\n", rep.Minimega.Error)
	}

	table := tablewriter.NewWriter(w)
	table.SetHeader([]string{"VLAN ID", "Alias", "Experiment", "Namespace", "States", "phēnix", "minimega", "VMs", "Issues"})

	for _, row := range rep.VLANs {
		table.Append([]string{
			strconv.Itoa(row.ID), row.Alias, row.Experiment, row.Namespace, strings.Join(row.States, ", "),
			optionalID(row.Sources.Phenix), optionalID(row.Sources.Minimega), strconv.Itoa(row.VMs),
			strings.Join(row.Issues, ", "),
		})
	}

	table.Render()

	if len(rep.Issues) == 0 {
		return
	}

	issues := tablewriter.NewWriter(w)
	issues.SetAutoWrapText(false)
	issues.SetHeader([]string{"Severity", "Kind", "Subject", "Message"})

	for _, i := range rep.Issues {
		subject := i.Namespace
		if len(i.Experiments) > 0 {
			subject = i.Experiments[0]
		}

		issues.Append([]string{i.Severity, i.Kind, subject, i.Message})
	}

	issues.Render()
}

func newVlanListCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "list",
		Short: "List the VLAN IDs of the cluster",
		Long: `List the VLAN IDs of the cluster

  Merges the VLAN pins and ranges of every experiment with the live aliases
  and the blacklist of minimega, one row for each owner, alias and VLAN ID.
  A second table lists the differences and conflicts.`,
		Example: `
  phenix vlan list
  phenix vlan list --experiment substation-a
  phenix vlan list --issues`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			in, _, err := vlan.Read(context.Background())
			if err != nil {
				return util.HumanizeError(err, "Unable to read the VLANs").Humanized()
			}

			rep := vlan.Filter(vlan.Merge(in, cliView()), vlan.Query{
				Experiment: mustGet(cmd.Flags().GetString, "experiment"), VLAN: 0, State: "",
				IssuesOnly: mustGet(cmd.Flags().GetBool, "issues"),
			})

			printVLANReport(cmd.OutOrStdout(), rep)

			return nil
		},
	}

	cmd.Flags().StringP("experiment", "e", "", "Show only this experiment or minimega namespace")
	cmd.Flags().Bool("issues", false, "Show only the rows with an issue")

	return cmd
}

func newVlanBlacklistCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "blacklist",
		Short: "View or change the minimega VLAN blacklist",
		Long: `View or change the minimega VLAN blacklist

  With no subcommand, lists the VLAN IDs that minimega does not give to any
  alias.`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			ids, removable, err := vlan.Blacklist(context.Background())
			if err != nil {
				return util.HumanizeError(err, "Unable to read the VLAN blacklist").Humanized()
			}

			for _, id := range ids {
				fmt.Fprintln(cmd.OutOrStdout(), id)
			}

			if !removable {
				fmt.Fprintln(cmd.ErrOrStderr(), "This minimega cannot remove a VLAN ID from its blacklist.")
			}

			return nil
		},
	}

	add := &cobra.Command{
		Use:   "add <vlan id>",
		Short: "Add a VLAN ID to the minimega blacklist",
		Args:  cobra.ExactArgs(1),
		RunE: func(_ *cobra.Command, args []string) error {
			id, err := parseCLIVLANID(args[0])
			if err != nil {
				return err
			}

			if err := vlan.AddToBlacklist(context.Background(), id); err != nil {
				return vlanRefusal(err, "Unable to blacklist VLAN "+args[0])
			}

			plog.Info(plog.TypeSystem, "VLAN blacklisted", "vlan", id)

			return nil
		},
	}

	remove := &cobra.Command{
		Use:   "remove <vlan id>",
		Short: "Remove a VLAN ID from the minimega blacklist",
		Long: `Remove a VLAN ID from the minimega blacklist

  Needs a minimega with the command "clear vlans blacklist <vlan>".`,
		Args: cobra.ExactArgs(1),
		RunE: func(_ *cobra.Command, args []string) error {
			id, err := parseCLIVLANID(args[0])
			if err != nil {
				return err
			}

			if err := vlan.RemoveFromBlacklist(context.Background(), id); err != nil {
				return vlanRefusal(err, "Unable to remove VLAN "+args[0]+" from the blacklist")
			}

			plog.Info(plog.TypeSystem, "VLAN removed from the blacklist", "vlan", id)

			return nil
		},
	}

	cmd.AddCommand(add, remove)

	return cmd
}

// vlanRefusal returns a refusal as it is, since its message is the answer,
// and humanizes any other error.
func vlanRefusal(err error, msg string) error {
	for _, refusal := range []error{
		vlan.ErrInvalid, vlan.ErrNotFound, vlan.ErrInUse, vlan.ErrUnsupported, vlan.ErrRunning, vlan.ErrNotRunning,
	} {
		if errors.Is(err, refusal) {
			return err
		}
	}

	return util.HumanizeError(err, "%s", msg).Humanized()
}

func newVlanReleaseCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "release <namespace>",
		Short: "Release the VLAN IDs of a minimega namespace that no experiment has",
		Long: `Release the VLAN IDs of a minimega namespace that no experiment has

  Clears the VLAN aliases of the namespace, so minimega can give the IDs to
  other experiments. It refuses a namespace that an experiment has, or whose
  VMs are on its VLAN IDs.`,
		Args: cobra.ExactArgs(1),
		RunE: func(_ *cobra.Command, args []string) error {
			if err := vlan.Release(context.Background(), args[0]); err != nil {
				return vlanRefusal(err, "Unable to release the VLANs of namespace "+args[0])
			}

			plog.Info(plog.TypeSystem, "VLANs released", "namespace", args[0])

			return nil
		},
	}
}

func newVlanPinLiveCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "pin-live <experiment name>",
		Short: "Pin the live VLAN IDs of a running experiment",
		Long: `Pin the live VLAN IDs of a running experiment

  Copies the VLAN IDs that minimega gave the aliases of a running experiment
  into its spec, so the next start gives the same IDs.`,
		Args:              cobra.ExactArgs(1),
		ValidArgsFunction: expNameCompletion(false),
		RunE: func(cmd *cobra.Command, args []string) error {
			live, err := vlan.PinLive(context.Background(), args[0])
			if err != nil {
				return vlanRefusal(err, "Unable to pin the live VLANs of the "+args[0]+" experiment")
			}

			printer.PrintTableOfVLANAliases(cmd.OutOrStdout(), map[string]map[string]int{args[0]: live})

			return nil
		},
	}
}
