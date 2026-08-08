package cli

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"

	"github.com/HelgeSverre/agentline/internal/localconfig"
	"github.com/spf13/cobra"
)

type Dependencies struct {
	Config localconfig.Store
	HTTP   *http.Client
}

type runner struct {
	ctx         context.Context
	in          io.Reader
	out, stderr io.Writer
	deps        Dependencies
	json        bool
}

func Run(ctx context.Context, args []string, in io.Reader, out, stderr io.Writer, deps Dependencies) int {
	r := runner{ctx: ctx, in: in, out: out, stderr: stderr, deps: deps}
	if len(args) > 0 && args[0] == "--json" {
		r.json, args = true, args[1:]
	}
	root := &cobra.Command{
		Use:           "agentline",
		SilenceErrors: true,
		SilenceUsage:  true,
		Args:          cobra.NoArgs,
	}
	root.SetArgs(args)
	root.SetIn(in)
	root.SetOut(out)
	root.SetErr(stderr)
	for name, run := range map[string]func([]string) error{
		"create": r.create, "join": r.join, "send": r.send, "read": r.read,
		"wait": r.wait, "done": r.done, "status": r.status, "mcp": r.mcp,
		"server": r.server, "local": r.local, "setup": r.setup, "doctor": r.doctor,
	} {
		root.AddCommand(&cobra.Command{Use: name, Args: cobra.ArbitraryArgs, DisableFlagParsing: true, RunE: func(run func([]string) error) func(*cobra.Command, []string) error {
			return func(_ *cobra.Command, commandArgs []string) error { return run(commandArgs) }
		}(run)})
	}
	if err := root.ExecuteContext(ctx); err != nil {
		r.fail(err)
		return 1
	}
	return 0
}

func (r runner) printJSON(value any) error { return json.NewEncoder(r.out).Encode(value) }
func (r runner) fail(err error) {
	if r.json {
		_ = json.NewEncoder(r.stderr).Encode(map[string]any{"error": map[string]string{"code": "cli_error", "message": err.Error()}})
		return
	}
	fmt.Fprintln(r.stderr, "agentline:", err)
}
