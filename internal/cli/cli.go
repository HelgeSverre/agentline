package cli

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"

	"github.com/HelgeSverre/agentline/internal/localconfig"
	"github.com/spf13/cobra"
)

var version = "dev"

type Dependencies struct {
	Config localconfig.Store
	HTTP   *http.Client
}

type app struct {
	ctx    context.Context
	in     io.Reader
	out    io.Writer
	stderr io.Writer
	deps   Dependencies
	json   bool
	style  style
}

func Run(ctx context.Context, args []string, in io.Reader, out, stderr io.Writer, deps Dependencies) int {
	a := &app{
		ctx:    ctx,
		in:     in,
		out:    out,
		stderr: stderr,
		deps:   deps,
		style:  newStyle(out),
	}
	root := a.newRoot()
	root.SetArgs(args)
	root.SetIn(in)
	root.SetOut(out)
	root.SetErr(stderr)
	root.SetFlagErrorFunc(func(cmd *cobra.Command, err error) error {
		return fmt.Errorf("%w; see 'agentline %s --help'", err, cmd.Name())
	})
	if err := root.ExecuteContext(ctx); err != nil {
		a.fail(err)
		return 1
	}
	return 0
}

func (a *app) newRoot() *cobra.Command {
	root := &cobra.Command{
		Use:           "agentline [command]",
		Short:         "Let two coding agents exchange messages",
		Long:          "agentline connects two coding agents through a temporary relay room.\nRooms need no accounts, expire automatically, and accept exactly two participants.",
		SilenceErrors: true,
		SilenceUsage:  true,
		Version:       version,
	}
	root.SetVersionTemplate("agentline {{.Version}}\n")
	root.PersistentFlags().BoolVar(&a.json, "json", false, "emit machine-readable JSON output")
	root.CompletionOptions.DisableDefaultCmd = true
	root.AddCommand(
		a.newCreateCommand(),
		a.newJoinCommand(),
		a.newSendCommand(),
		a.newReadCommand(),
		a.newWaitCommand(),
		a.newDoneCommand(),
		a.newStatusCommand(),
		a.newListCommand(),
		a.newMCPCommand(),
		a.newServerCommand(),
		a.newLocalCommand(),
		a.newSetupCommand(),
		a.newDoctorCommand(),
	)
	return root
}

func (a *app) fail(err error) {
	if a.json {
		_ = json.NewEncoder(a.stderr).Encode(map[string]any{
			"error": map[string]string{"code": "cli_error", "message": err.Error()},
		})
		return
	}
	fmt.Fprintln(a.stderr, "agentline:", err)
	if hint := usageHint(err.Error()); hint != "" {
		fmt.Fprintln(a.stderr, hint)
	}
}

func usageHint(message string) string {
	switch {
	case strings.Contains(message, "unknown command"),
		strings.Contains(message, "does not accept any arguments"):
		return "Run 'agentline --help' to see available commands."
	default:
		return ""
	}
}
