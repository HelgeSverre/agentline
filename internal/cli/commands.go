package cli

import (
	"context"
	"flag"
	"fmt"
	"io"
	"net"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/HelgeSverre/agentline/internal/client"
	"github.com/HelgeSverre/agentline/internal/localserver"
	"github.com/HelgeSverre/agentline/internal/mcpserver"
	"github.com/HelgeSverre/agentline/internal/model"
	"github.com/HelgeSverre/agentline/internal/relay"
	"github.com/HelgeSverre/agentline/internal/store"
)

func (r runner) flags(name string) *flag.FlagSet {
	f := flag.NewFlagSet(name, flag.ContinueOnError)
	f.SetOutput(io.Discard)
	return f
}

func parseFlags(f *flag.FlagSet, args []string) error {
	flags, positional := make([]string, 0, len(args)), make([]string, 0, len(args))
	for i := 0; i < len(args); i++ {
		arg := args[i]
		if arg == "--" {
			positional = append(positional, args[i+1:]...)
			break
		}
		if len(arg) > 1 && arg[0] == '-' {
			flags = append(flags, arg)
			name := strings.TrimLeft(strings.SplitN(arg, "=", 2)[0], "-")
			value := f.Lookup(name)
			if !strings.Contains(arg, "=") && value != nil {
				_, boolean := value.Value.(interface{ IsBoolFlag() bool })
				if !boolean && i+1 < len(args) {
					i++
					flags = append(flags, args[i])
				}
			}
			continue
		}
		positional = append(positional, arg)
	}
	return f.Parse(append(flags, positional...))
}

type durationValue time.Duration

func (d *durationValue) String() string { return time.Duration(*d).String() }
func (d *durationValue) Set(value string) error {
	var duration time.Duration
	var err error
	if strings.HasSuffix(value, "d") && !strings.Contains(value[:len(value)-1], "d") {
		var days float64
		days, err = strconv.ParseFloat(strings.TrimSuffix(value, "d"), 64)
		duration = time.Duration(days * float64(24*time.Hour))
	} else {
		duration, err = time.ParseDuration(value)
	}
	if err != nil {
		return err
	}
	*d = durationValue(duration)
	return nil
}

func hasFlag(args []string, name string) bool {
	for _, arg := range args {
		if arg == "--" {
			return false
		}
		if arg == "-"+name || arg == "--"+name || strings.HasPrefix(arg, "-"+name+"=") || strings.HasPrefix(arg, "--"+name+"=") {
			return true
		}
	}
	return false
}

func (r runner) create(args []string) error {
	f := r.flags("create")
	participant := f.String("name", "agent", "participant name")
	roomName := f.String("room-name", "agentline", "room name")
	ttl := durationValue(24 * time.Hour)
	f.Var(&ttl, "ttl", "room lifetime")
	server := f.String("server", "", "relay URL")
	local := f.Bool("local", false, "use loopback relay")
	explicitServer := hasFlag(args, "server")
	if err := parseFlags(f, args); err != nil {
		return err
	}
	if f.NArg() != 0 {
		return fmt.Errorf("usage: agentline create [flags]")
	}
	if *local && explicitServer {
		return fmt.Errorf("--local and --server cannot be used together")
	}
	if time.Duration(ttl) <= 0 || time.Duration(ttl) > 7*24*time.Hour {
		return fmt.Errorf("ttl must be greater than zero and at most 7d")
	}
	if err := r.deps.Config.Preflight(); err != nil {
		return err
	}
	config, err := r.deps.Config.Load()
	if err != nil {
		return err
	}
	if *server == "" {
		*server = config.ServerURL
	}
	if *local {
		executable, err := os.Executable()
		if err != nil {
			return err
		}
		*server, err = (localserver.Manager{Config: r.deps.Config, Executable: executable}).Ensure(r.ctx)
		if err != nil {
			return err
		}
	}
	if err := client.ValidateOrigin(*server); err != nil {
		return fmt.Errorf("invalid server URL: %w", err)
	}
	result, err := client.New(*server, "", r.deps.HTTP).CreateRoom(r.ctx, *roomName, *participant, time.Duration(ttl))
	if err != nil {
		return err
	}
	credential := model.RoomCredential{RoomID: result.Room.ID, RoomName: result.Room.Name, ServerURL: *server, ParticipantID: result.Participant.ID, Token: result.ParticipantToken}
	if err := r.deps.Config.SaveRoom(credential); err != nil {
		return err
	}
	if r.json {
		return r.printJSON(result)
	}
	fmt.Fprintf(r.out, "Room: %s\nInvite: %s\nExpires: %s\n", result.Room.Name, result.InviteURL, result.Room.ExpiresAt.Format(time.RFC3339))
	return nil
}

func (r runner) join(args []string) error {
	f := r.flags("join")
	name := f.String("name", "agent", "participant name")
	if err := parseFlags(f, args); err != nil {
		return err
	}
	if f.NArg() != 1 {
		return fmt.Errorf("usage: agentline join INVITE [--name NAME]")
	}
	if err := r.deps.Config.Preflight(); err != nil {
		return err
	}
	invite := f.Arg(0)
	token, err := client.InviteToken(invite)
	if err != nil || token == "" {
		return fmt.Errorf("invalid invite URL")
	}
	cut := strings.Index(invite, "/join/")
	origin := invite[:cut]
	result, err := client.New(origin, "", r.deps.HTTP).ClaimInvite(r.ctx, invite, *name)
	if err != nil {
		return err
	}
	credential := model.RoomCredential{RoomID: result.Room.ID, RoomName: result.Room.Name, ServerURL: origin, ParticipantID: result.Participant.ID, Token: result.ParticipantToken}
	if err := r.deps.Config.SaveRoom(credential); err != nil {
		return err
	}
	if r.json {
		return r.printJSON(result)
	}
	fmt.Fprintf(r.out, "Joined room %s\n", result.Room.Name)
	return nil
}

func (r runner) room(args []string, minimum, maximum int) (model.RoomCredential, []string, error) {
	if len(args) < minimum || len(args) > maximum {
		return model.RoomCredential{}, nil, fmt.Errorf("invalid arguments")
	}
	handle := ""
	if len(args) == maximum {
		handle, args = args[0], args[1:]
	}
	credential, err := r.deps.Config.LoadRoom(handle)
	return credential, args, err
}

func (r runner) send(args []string) error {
	f := r.flags("send")
	reply := f.String("reply-to", "", "message ID being answered")
	if err := parseFlags(f, args); err != nil {
		return err
	}
	credential, rest, err := r.room(f.Args(), 1, 2)
	if err != nil {
		return fmt.Errorf("send: %w", err)
	}
	message, err := client.New(credential.ServerURL, credential.Token, r.deps.HTTP).Send(r.ctx, credential.RoomID, "", rest[0], *reply)
	if err != nil {
		return err
	}
	if r.json {
		return r.printJSON(message)
	}
	fmt.Fprintf(r.out, "Sent message %s (sequence %d)\n", message.ID, message.Sequence)
	return nil
}

func (r runner) read(args []string) error {
	f := r.flags("read")
	after := f.Int64("after", -1, "sequence cursor")
	if err := parseFlags(f, args); err != nil {
		return err
	}
	credential, _, err := r.room(f.Args(), 0, 1)
	if err != nil {
		return fmt.Errorf("read: %w", err)
	}
	cursor := credential.Cursor
	if *after >= 0 {
		cursor = *after
	}
	messages, err := client.New(credential.ServerURL, credential.Token, r.deps.HTTP).Read(r.ctx, credential.RoomID, cursor)
	if err != nil {
		return err
	}
	if err := r.advance(credential, messages); err != nil {
		return err
	}
	if r.json {
		return r.printJSON(map[string]any{"messages": messages})
	}
	for _, m := range messages {
		fmt.Fprintf(r.out, "%d %s: %s\n", m.Sequence, m.SenderName, m.Body)
	}
	return nil
}

func (r runner) wait(args []string) error {
	f := r.flags("wait")
	after := f.Int64("after", -1, "sequence cursor")
	timeout := f.Duration("timeout", 60*time.Second, "maximum wait")
	if err := parseFlags(f, args); err != nil {
		return err
	}
	credential, _, err := r.room(f.Args(), 0, 1)
	if err != nil {
		return fmt.Errorf("wait: %w", err)
	}
	cursor := credential.Cursor
	if *after >= 0 {
		cursor = *after
	}
	result, err := client.New(credential.ServerURL, credential.Token, r.deps.HTTP).Wait(r.ctx, credential.RoomID, cursor, *timeout)
	if err != nil {
		return err
	}
	sequence := credential.Cursor
	if result.Message != nil && result.Message.Sequence > credential.Cursor {
		sequence = result.Message.Sequence
	} else if result.Message == nil && result.Sequence > credential.Cursor {
		sequence = result.Sequence
	}
	if sequence > credential.Cursor {
		if err := r.deps.Config.AdvanceCursor(credential.RoomID, sequence); err != nil {
			return err
		}
	}
	if r.json {
		return r.printJSON(result)
	}
	switch result.Status {
	case "message":
		fmt.Fprintf(r.out, "%d %s: %s\n", result.Message.Sequence, result.Message.SenderName, result.Message.Body)
	case "timeout":
		fmt.Fprintln(r.out, result.Instruction)
	case "done":
		fmt.Fprintln(r.out, "Room is done")
	}
	return nil
}

func (r runner) done(args []string) error {
	f := r.flags("done")
	if err := parseFlags(f, args); err != nil {
		return err
	}
	credential, _, err := r.room(f.Args(), 0, 1)
	if err != nil {
		return fmt.Errorf("done: %w", err)
	}
	message, err := client.New(credential.ServerURL, credential.Token, r.deps.HTTP).Done(r.ctx, credential.RoomID, "")
	if err != nil {
		return err
	}
	if err := r.deps.Config.AdvanceCursor(credential.RoomID, message.Sequence); err != nil {
		return err
	}
	if r.json {
		return r.printJSON(message)
	}
	fmt.Fprintln(r.out, "Room ended")
	return nil
}

func (r runner) status(args []string) error {
	f := r.flags("status")
	if err := parseFlags(f, args); err != nil {
		return err
	}
	credential, _, err := r.room(f.Args(), 0, 1)
	if err != nil {
		return fmt.Errorf("status: %w", err)
	}
	room, err := client.New(credential.ServerURL, credential.Token, r.deps.HTTP).Room(r.ctx, credential.RoomID)
	if err != nil {
		return err
	}
	if r.json {
		return r.printJSON(room)
	}
	fmt.Fprintf(r.out, "Room: %s\nStatus: %s\nExpires: %s\n", room.Name, room.Status, room.ExpiresAt.Format(time.RFC3339))
	return nil
}

func (r runner) advance(credential model.RoomCredential, messages []model.Message) error {
	sequence := credential.Cursor
	for _, message := range messages {
		if message.Sequence > sequence {
			sequence = message.Sequence
		}
	}
	return r.deps.Config.AdvanceCursor(credential.RoomID, sequence)
}

func (r runner) mcp(args []string) error {
	if len(args) != 0 {
		return fmt.Errorf("usage: agentline mcp")
	}
	return mcpserver.Run(r.ctx, mcpserver.Dependencies{Config: r.deps.Config, HTTP: r.deps.HTTP})
}

func (r runner) server(args []string) error {
	f := r.flags("server")
	listen := f.String("listen", ":8080", "listen address")
	publicURL := f.String("public-url", "http://localhost:8080", "external URL")
	data := f.String("data", "agentline.db", "SQLite path")
	localInstance := f.String("local-instance", "", "managed local relay identity")
	explicitPublicURL := hasFlag(args, "public-url")
	if err := parseFlags(f, args); err != nil {
		return err
	}
	if f.NArg() != 0 {
		return fmt.Errorf("usage: agentline server [flags]")
	}
	if err := client.ValidateOrigin(*publicURL); err != nil {
		return fmt.Errorf("invalid public URL: %w", err)
	}
	db, err := store.OpenSQLite(*data, nil)
	if err != nil {
		return err
	}
	listener, err := net.Listen("tcp", *listen)
	if err != nil {
		db.Close()
		return err
	}
	if !explicitPublicURL {
		if address, ok := listener.Addr().(*net.TCPAddr); ok && address.IP.IsLoopback() {
			*publicURL = "http://127.0.0.1:" + strconv.Itoa(address.Port)
		}
	}
	address, loopback := listener.Addr().(*net.TCPAddr)
	loopback = loopback && address.IP.IsLoopback()
	if *localInstance != "" && !loopback {
		listener.Close()
		db.Close()
		return fmt.Errorf("--local-instance requires a loopback listener")
	}
	if r.json {
		_ = r.printJSON(map[string]string{"status": "listening", "address": listener.Addr().String(), "public_url": *publicURL})
	} else {
		fmt.Fprintln(r.out, "Listening on", listener.Addr())
	}
	handler := relay.NewHandler(db, relay.Config{PublicURL: strings.TrimRight(*publicURL, "/")}, nil)
	serveCtx := r.ctx
	if *localInstance != "" {
		var cancel context.CancelFunc
		serveCtx, cancel = context.WithCancel(r.ctx)
		defer cancel()
		handler = localserver.ManagementHandler(*localInstance, handler, cancel)
	}
	return relay.Serve(serveCtx, listener, handler, db)
}

func (r runner) local(args []string) error {
	if len(args) != 1 || args[0] != "stop" {
		return fmt.Errorf("usage: agentline local stop")
	}
	if err := (localserver.Manager{Config: r.deps.Config}).Stop(r.ctx); err != nil {
		return err
	}
	if r.json {
		return r.printJSON(map[string]string{"status": "stopped"})
	}
	fmt.Fprintln(r.out, "Local relay stopped")
	return nil
}
