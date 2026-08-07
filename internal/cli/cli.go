package cli

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"

	"github.com/HelgeSverre/agentline/internal/localconfig"
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
	if len(args) == 0 {
		r.fail(fmt.Errorf("usage: agentline [--json] <create|join|send|read|wait|done|status|server>"))
		return 2
	}
	var err error
	switch args[0] {
	case "create":
		err = r.create(args[1:])
	case "join":
		err = r.join(args[1:])
	case "send":
		err = r.send(args[1:])
	case "read":
		err = r.read(args[1:])
	case "wait":
		err = r.wait(args[1:])
	case "done":
		err = r.done(args[1:])
	case "status":
		err = r.status(args[1:])
	case "server":
		err = r.server(args[1:])
	default:
		err = fmt.Errorf("unknown command %q", args[0])
	}
	if err != nil {
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
