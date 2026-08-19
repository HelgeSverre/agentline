package cli

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/HelgeSverre/agentline/internal/channel"
	"github.com/HelgeSverre/agentline/internal/client"
	"github.com/HelgeSverre/agentline/internal/localconfig"
	"github.com/HelgeSverre/agentline/internal/localserver"
	"github.com/HelgeSverre/agentline/internal/mcpserver"
	"github.com/HelgeSverre/agentline/internal/model"
	"github.com/HelgeSverre/agentline/internal/relay"
	setupconfig "github.com/HelgeSverre/agentline/internal/setup"
	"github.com/HelgeSverre/agentline/internal/store"
	"github.com/spf13/cobra"
)

func (a *app) newCreateCommand() *cobra.Command {
	o := &createOpts{}
	o.ttl = durationValue(24 * time.Hour)
	cmd := &cobra.Command{
		Use:   "create",
		Short: "create a room and print a reusable invite",
		Long:  "Create a temporary room and store your participant credential. Share the printed invite URL with collaborators; each claim gets a separate credential until an optional capacity is reached.",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			return a.create(o, cmd.Flags().Changed("local"), cmd.Flags().Changed("server"), cmd.Flags().Changed("max-participants"))
		},
	}
	cmd.Flags().StringVar(&o.name, "name", "agent", "participant name")
	cmd.Flags().StringVar(&o.roomName, "room-name", "agentline", "room name")
	cmd.Flags().Var(&o.ttl, "ttl", "room lifetime, for example 24h or 1d (max 7d)")
	cmd.Flags().StringVar(&o.server, "server", "", "relay server URL; defaults to the saved server")
	cmd.Flags().BoolVar(&o.local, "local", false, "use the managed loopback relay")
	cmd.Flags().IntVar(&o.maxParticipants, "max-participants", 0, "maximum participants; omit for no limit")
	return cmd
}

func (a *app) create(o *createOpts, localExplicit, serverExplicit, capacityExplicit bool) error {
	if o.local && serverExplicit {
		return errors.New("--local and --server cannot be used together")
	}
	if time.Duration(o.ttl) <= 0 || time.Duration(o.ttl) > 7*24*time.Hour {
		return errors.New("ttl must be greater than zero and at most 7d")
	}
	config, err := a.deps.Config.Load()
	if err != nil {
		return err
	}
	serverURL := o.server
	if serverURL == "" {
		serverURL = config.ServerURL
	}
	if o.local {
		executable, err := os.Executable()
		if err != nil {
			return err
		}
		serverURL, err = (localserver.Manager{Config: a.deps.Config, Executable: executable}).Ensure(a.ctx)
		if err != nil {
			return err
		}
	}
	if err := client.ValidateOrigin(serverURL); err != nil {
		return fmt.Errorf("invalid server URL: %w", err)
	}
	var capacity *int
	if capacityExplicit && o.maxParticipants <= 0 {
		return errors.New("max-participants must be a positive integer")
	}
	if o.maxParticipants > 0 {
		capacity = &o.maxParticipants
	}
	result, err := client.New(serverURL, "", a.deps.HTTP).CreateRoom(a.ctx, o.roomName, o.name, time.Duration(o.ttl), capacity)
	if err != nil {
		return err
	}
	credential := model.RoomCredential{RoomID: result.Room.ID, RoomName: result.Room.Name, ServerURL: serverURL, ParticipantID: result.Participant.ID, Token: result.ParticipantToken}
	if err := a.deps.Config.SaveRoom(credential); err != nil {
		return err
	}
	if a.json {
		return a.printJSON(result)
	}
	fmt.Fprintf(a.out, "Room: %s\nInvite: %s\nInspect: %s\nExpires: %s\n", result.Room.Name, result.InviteURL, result.InspectURL, result.Room.ExpiresAt.Format(time.RFC3339))
	return nil
}

func (a *app) newJoinCommand() *cobra.Command {
	o := &joinOpts{}
	cmd := &cobra.Command{
		Use:   "join INVITE",
		Short: "claim a room using its one-use invite URL",
		Long:  "Claim an invite that another agent's create command produced. Pass the full invite URL beginning with your relay's /join/ path. The token inside INVITE is a secret; keep it out of logs.",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			return a.join(o, args[0])
		},
	}
	cmd.Flags().StringVar(&o.name, "name", "agent", "participant name")
	return cmd
}

func (a *app) join(o *joinOpts, invite string) error {
	token, err := client.InviteToken(invite)
	if err != nil || token == "" {
		return errors.New("invalid invite URL")
	}
	cut := strings.Index(invite, "/join/")
	origin := invite[:cut]
	result, err := client.New(origin, "", a.deps.HTTP).ClaimInvite(a.ctx, invite, o.name)
	if err != nil {
		return err
	}
	credential := model.RoomCredential{RoomID: result.Room.ID, RoomName: result.Room.Name, ServerURL: origin, ParticipantID: result.Participant.ID, Token: result.ParticipantToken}
	if err := a.deps.Config.SaveRoom(credential); err != nil {
		return err
	}
	if a.json {
		return a.printJSON(result)
	}
	fmt.Fprintf(a.out, "Joined room %s\n", result.Room.Name)
	return nil
}

func (a *app) newSendCommand() *cobra.Command {
	o := &sendOpts{}
	cmd := &cobra.Command{
		Use:   "send [ROOM] MESSAGE",
		Short: "send a message into a room",
		Long:  "Send MESSAGE to ROOM, which defaults to your only saved room. Set --reply-to to mark the message as an answer to a specific message ID.",
		Args:  cobra.RangeArgs(1, 2),
		RunE: func(cmd *cobra.Command, args []string) error {
			return a.send(o, args)
		},
	}
	cmd.Flags().StringVar(&o.replyTo, "reply-to", "", "message ID being answered")
	cmd.Flags().StringVar(&o.to, "to", "", "participant ID for a private message")
	cmd.Flags().StringVar(&o.messageID, "message-id", "", "stable ID reused when retrying this logical message")
	return cmd
}

func (a *app) send(o *sendOpts, args []string) error {
	credential, rest, err := a.room(args, 1, 2)
	if err != nil {
		return fmt.Errorf("send: %w", err)
	}
	message, err := client.New(credential.ServerURL, credential.Token, a.deps.HTTP).Send(a.ctx, credential.RoomID, o.messageID, rest[0], o.replyTo, o.to)
	if err != nil {
		return err
	}
	if err := a.deps.Config.AdvanceCursor(credential.RoomID, message.Sequence); err != nil {
		return err
	}
	if a.json {
		return a.printJSON(message)
	}
	fmt.Fprintf(a.out, "Sent message %s (sequence %d)\n", message.ID, message.Sequence)
	return nil
}

func (a *app) newReadCommand() *cobra.Command {
	o := &readWaitOpts{}
	cmd := &cobra.Command{
		Use:   "read [ROOM]",
		Short: "read messages that arrived after the stored cursor",
		Long:  "Print messages in ROOM newer than your stored cursor, then advance it. --after overrides the cursor for this call only.",
		Args:  cobra.RangeArgs(0, 1),
		RunE: func(cmd *cobra.Command, args []string) error {
			return a.read(o, args)
		},
	}
	cmd.Flags().Int64Var(&o.after, "after", -1, "sequence cursor; defaults to the saved cursor")
	return cmd
}

func (a *app) read(o *readWaitOpts, args []string) error {
	credential, _, err := a.room(args, 0, 1)
	if err != nil {
		return fmt.Errorf("read: %w", err)
	}
	cursor := credential.Cursor
	if o.after >= 0 {
		cursor = o.after
	}
	result, err := client.New(credential.ServerURL, credential.Token, a.deps.HTTP).Read(a.ctx, credential.RoomID, cursor)
	if err != nil {
		return err
	}
	if err := a.advance(credential, result.Messages, result.Cursor); err != nil {
		return err
	}
	if a.json {
		return a.printJSON(result)
	}
	for _, m := range result.Messages {
		fmt.Fprintf(a.out, "%d %s: %s\n", m.Sequence, m.SenderName, m.Body)
	}
	return nil
}

func (a *app) newWaitCommand() *cobra.Command {
	o := &readWaitOpts{}
	cmd := &cobra.Command{
		Use:   "wait [ROOM]",
		Short: "wait for the next message or room end",
		Long:  "Block until another participant sends a message, ROOM completes, or --timeout elapses. A timeout is ordinary: repeat the wait while a response is still expected.",
		Args:  cobra.RangeArgs(0, 1),
		RunE: func(cmd *cobra.Command, args []string) error {
			return a.wait(o, args)
		},
	}
	cmd.Flags().Int64Var(&o.after, "after", -1, "sequence cursor; defaults to the saved cursor")
	cmd.Flags().DurationVar(&o.timeout, "timeout", 60*time.Second, "maximum wait before returning timeout")
	return cmd
}

func (a *app) wait(o *readWaitOpts, args []string) error {
	credential, _, err := a.room(args, 0, 1)
	if err != nil {
		return fmt.Errorf("wait: %w", err)
	}
	cursor := credential.Cursor
	if o.after >= 0 {
		cursor = o.after
	}
	result, err := client.New(credential.ServerURL, credential.Token, a.deps.HTTP).Wait(a.ctx, credential.RoomID, cursor, o.timeout)
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
		if err := a.deps.Config.AdvanceCursor(credential.RoomID, sequence); err != nil {
			return err
		}
	}
	if a.json {
		return a.printJSON(result)
	}
	switch result.Status {
	case "message":
		fmt.Fprintf(a.out, "%d %s: %s\n", result.Message.Sequence, result.Message.SenderName, result.Message.Body)
	case "timeout":
		fmt.Fprintln(a.out, result.Instruction)
	case "done":
		fmt.Fprintln(a.out, "Room is done")
	}
	return nil
}

func (a *app) newDoneCommand() *cobra.Command {
	o := &doneOpts{}
	cmd := &cobra.Command{
		Use:   "done [ROOM]",
		Short: "close a room and stop new writes",
		Long:  "Write the done marker for ROOM. Message history stays readable. --message-id makes a retry idempotent.",
		Args:  cobra.RangeArgs(0, 1),
		RunE: func(cmd *cobra.Command, args []string) error {
			return a.done(o, args)
		},
	}
	cmd.Flags().StringVar(&o.messageID, "message-id", "", "stable ID reused when retrying this logical operation")
	return cmd
}

func (a *app) done(o *doneOpts, args []string) error {
	credential, _, err := a.room(args, 0, 1)
	if err != nil {
		return fmt.Errorf("done: %w", err)
	}
	message, err := client.New(credential.ServerURL, credential.Token, a.deps.HTTP).Done(a.ctx, credential.RoomID, o.messageID)
	if err != nil {
		return err
	}
	if err := a.deps.Config.AdvanceCursor(credential.RoomID, message.Sequence); err != nil {
		return err
	}
	if a.json {
		return a.printJSON(message)
	}
	fmt.Fprintln(a.out, "Room ended")
	return nil
}

func (a *app) newStatusCommand() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "status [ROOM]",
		Short: "show whether the room is open or done",
		Long:  "Query ROOM's name, status, and expiry from the relay.",
		Args:  cobra.RangeArgs(0, 1),
		RunE: func(cmd *cobra.Command, args []string) error {
			return a.status(args)
		},
	}
	return cmd
}

func (a *app) status(args []string) error {
	credential, _, err := a.room(args, 0, 1)
	if err != nil {
		return fmt.Errorf("status: %w", err)
	}
	room, err := client.New(credential.ServerURL, credential.Token, a.deps.HTTP).Room(a.ctx, credential.RoomID)
	if err != nil {
		return err
	}
	if a.json {
		return a.printJSON(room)
	}
	fmt.Fprintf(a.out, "Room: %s\nStatus: %s\nExpires: %s\n", room.Name, room.Status, room.ExpiresAt.Format(time.RFC3339))
	return nil
}

func (a *app) newListCommand() *cobra.Command {
	return &cobra.Command{
		Use:   "list",
		Short: "list saved rooms on this machine",
		Long:  "Show the rooms this agentline created or joined, their relay servers, and their last-read cursor. Participant tokens never print.",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			return a.list()
		},
	}
}

func (a *app) list() error {
	rooms, err := a.deps.Config.ListRooms()
	if err != nil {
		return err
	}
	type roomInfo struct {
		RoomID    string `json:"room_id"`
		Name      string `json:"name"`
		ServerURL string `json:"server_url"`
		Cursor    int64  `json:"cursor"`
	}
	info := make([]roomInfo, 0, len(rooms))
	width := 0
	for _, room := range rooms {
		info = append(info, roomInfo{room.RoomID, room.RoomName, room.ServerURL, room.Cursor})
		if len(room.RoomName) > width {
			width = len(room.RoomName)
		}
	}
	if a.json {
		return a.printJSON(info)
	}
	if len(info) == 0 {
		fmt.Fprintln(a.out, "No saved rooms yet. Run 'agentline create' or 'agentline join INVITE'.")
		return nil
	}
	for _, room := range info {
		fmt.Fprintf(a.out, "%-*s  %s  cursor=%d\n", width, a.style.bold(room.Name), room.ServerURL, room.Cursor)
	}
	return nil
}

func (a *app) newMCPCommand() *cobra.Command {
	return &cobra.Command{
		Use:   "mcp",
		Short: "serve the stdio MCP interface for harnesses",
		Long:  "Run the portable Model Context Protocol server over stdio. Harnesses with an MCP client use this for create_room, join_room, send_message, read_messages, wait_for_message, end_conversation, and get_room_status.",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			return mcpserver.Run(a.ctx, mcpserver.Dependencies{Config: a.deps.Config, HTTP: a.deps.HTTP})
		},
	}
}

func (a *app) newChannelCommand() *cobra.Command {
	o := &channelOpts{}
	cmd := &cobra.Command{
		Use:   "channel",
		Short: "serve the experimental Claude Channel adapter",
		Long: "Run the experimental Claude Channel adapter over stdio. Unlike 'agentline mcp', which delivers only while the session polls, a Channel pushes collaborator messages into an idle running Claude Code session and exposes an agentline_reply tool.\n\n" +
			"Channels are a Claude Code research preview. Custom channels are not on the approved allowlist, so start Claude with:\n\n" +
			"  claude --dangerously-load-development-channels server:agentline-channel\n\n" +
			"That flag skips the allowlist only; the channelsEnabled organization policy still applies.",
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			return channel.Run(a.ctx, a.in, a.out, channel.Dependencies{Config: a.deps.Config, HTTP: a.deps.HTTP, Room: o.room})
		},
	}
	cmd.Flags().StringVar(&o.room, "room", "", "watch only this saved room; default watches every saved room")
	return cmd
}

func (a *app) newServerCommand() *cobra.Command {
	o := &serverOpts{}
	cmd := &cobra.Command{
		Use:   "server",
		Short: "run the relay server",
		Long:  "Run the HTTP relay with SQLite persistence and the embedded website. --public-url controls the invites and join links this server generates.",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			return a.server(o, cmd.Flags().Changed("public-url"))
		},
	}
	cmd.Flags().StringVar(&o.listen, "listen", ":8080", "listen address")
	cmd.Flags().StringVar(&o.publicURL, "public-url", "http://localhost:8080", "external URL for invites and join links")
	cmd.Flags().StringVar(&o.data, "data", "agentline.db", "SQLite database path")
	cmd.Flags().StringVar(&o.localInstance, "local-instance", "", "internal flag for the managed loopback relay")
	return cmd
}

func (a *app) server(o *serverOpts, publicURLExplicit bool) error {
	if err := client.ValidateOrigin(o.publicURL); err != nil {
		return fmt.Errorf("invalid public URL: %w", err)
	}
	db, err := store.OpenSQLite(o.data, nil)
	if err != nil {
		return err
	}
	listener, err := net.Listen("tcp", o.listen)
	if err != nil {
		db.Close()
		return err
	}
	if !publicURLExplicit {
		if address, ok := listener.Addr().(*net.TCPAddr); ok && address.IP.IsLoopback() {
			o.publicURL = "http://127.0.0.1:" + strconv.Itoa(address.Port)
		}
	}
	address, loopback := listener.Addr().(*net.TCPAddr)
	loopback = loopback && address.IP.IsLoopback()
	if o.localInstance != "" && !loopback {
		listener.Close()
		db.Close()
		return errors.New("--local-instance requires a loopback listener")
	}
	if a.json {
		_ = a.printJSON(map[string]string{"status": "listening", "address": listener.Addr().String(), "public_url": o.publicURL})
	} else {
		fmt.Fprintln(a.out, "Listening on", listener.Addr())
	}
	handler := relay.NewHandler(db, relay.Config{PublicURL: strings.TrimRight(o.publicURL, "/")}, nil)
	serveCtx := a.ctx
	if o.localInstance != "" {
		var cancel context.CancelFunc
		serveCtx, cancel = context.WithCancel(a.ctx)
		defer cancel()
		handler = localserver.ManagementHandler(o.localInstance, handler, cancel)
	}
	return relay.Serve(serveCtx, listener, handler, db)
}

func (a *app) newLocalCommand() *cobra.Command {
	local := &cobra.Command{
		Use:   "local",
		Short: "manage the managed loopback relay",
		Long:  "Stop the loopback relay that 'agentline create --local' starts on demand. Starting is opportunistic and needs no command.",
		RunE: func(cmd *cobra.Command, args []string) error {
			if len(args) > 0 {
				return fmt.Errorf(`unknown command %q for %q`, args[0], cmd.CommandPath())
			}
			return cmd.Help()
		},
	}
	stop := &cobra.Command{
		Use:   "stop",
		Short: "stop the loopback relay",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			return a.localStop()
		},
	}
	local.AddCommand(stop)
	return local
}

func (a *app) localStop() error {
	if err := (localserver.Manager{Config: a.deps.Config}).Stop(a.ctx); err != nil {
		return err
	}
	if a.json {
		return a.printJSON(map[string]string{"status": "stopped"})
	}
	fmt.Fprintln(a.out, "Local relay stopped")
	return nil
}

func (a *app) newSetupCommand() *cobra.Command {
	o := &setupOpts{}
	cmd := &cobra.Command{
		Use:   "setup TARGET",
		Short: "install or remove harness integrations",
		Long:  "Plan or apply integration changes for a coding-agent harness (claude, codex, amp, pi, opencode, or mcp): skills, MCP registration, and native adapters. Shows pending changes and asks before writing unless --yes is set. --remove deletes only Agentline-owned entries.",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			return a.setup(o, args[0])
		},
	}
	cmd.Flags().BoolVar(&o.yes, "yes", false, "apply without confirmation")
	cmd.Flags().BoolVar(&o.remove, "remove", false, "remove Agentline-owned setup")
	return cmd
}

func (a *app) setup(o *setupOpts, target string) error {
	home, err := os.UserHomeDir()
	if err != nil {
		return err
	}
	executable, err := os.Executable()
	if err != nil {
		return err
	}
	executable, err = filepath.Abs(executable)
	if err != nil {
		return err
	}
	plan, err := setupconfig.BuildPlan(target, home, executable, o.remove)
	if err != nil {
		return err
	}
	if warning := setupconfig.HarnessVersionWarning(target); warning != "" {
		plan.Warnings = append(plan.Warnings, warning)
	}
	type previewChange struct {
		Path        string `json:"path"`
		Description string `json:"description"`
		Action      string `json:"action"`
	}
	changes := make([]previewChange, 0, len(plan.Changes))
	for _, change := range plan.Changes {
		action := "create"
		if len(change.After) == 0 {
			action = "remove"
		} else if len(change.Before) > 0 {
			action = "update"
		}
		changes = append(changes, previewChange{change.Path, change.Description, action})
	}
	if a.json {
		applied := false
		if o.yes && len(plan.Changes) > 0 {
			if err := setupconfig.Apply(plan); err != nil {
				return err
			}
			applied = true
		}
		return a.printJSON(map[string]any{"target": plan.Target, "changes": changes, "warnings": plan.Warnings, "applied": applied})
	}
	if len(changes) == 0 {
		fmt.Fprintln(a.out, "No changes needed.")
		return nil
	}
	fmt.Fprintln(a.out, "Planned changes:")
	for _, change := range changes {
		fmt.Fprintf(a.out, "- %s %s: %s\n", change.Action, change.Path, change.Description)
	}
	for _, warning := range plan.Warnings {
		fmt.Fprintln(a.out, "Warning:", warning)
	}
	if !o.yes {
		fmt.Fprint(a.out, "Apply these changes? [y/N] ")
		var answer string
		fmt.Fscanln(a.in, &answer)
		if !strings.EqualFold(answer, "y") && !strings.EqualFold(answer, "yes") {
			return errors.New("setup not confirmed")
		}
	}
	if err := setupconfig.Apply(plan); err != nil {
		return err
	}
	fmt.Fprintln(a.out, "Setup applied.")
	return nil
}

func (a *app) newDoctorCommand() *cobra.Command {
	o := &doctorOpts{}
	cmd := &cobra.Command{
		Use:   "doctor",
		Short: "verify local setup and relay connectivity",
		Long:  "Check the binary, relay reachability, saved credentials, skill discovery, MCP registration, and native adapters. --target limits checks to one harness.",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			return a.doctor(o)
		},
	}
	cmd.Flags().StringVar(&o.target, "target", "all", "target harness or all")
	cmd.Flags().StringVar(&o.server, "server", "", "relay URL")
	return cmd
}

func (a *app) doctor(o *doctorOpts) error {
	home, err := os.UserHomeDir()
	if err != nil {
		return err
	}
	executable, err := os.Executable()
	if err != nil {
		return err
	}
	executable, err = filepath.Abs(executable)
	if err != nil {
		return err
	}
	serverURL := o.server
	if serverURL == "" {
		config, err := a.deps.Config.Load()
		if err != nil {
			return err
		}
		serverURL = config.ServerURL
	}
	report := setupconfig.Doctor(a.ctx, o.target, home, executable, serverURL)
	if a.json {
		return a.printJSON(report)
	}
	for _, check := range report.Checks {
		fmt.Fprintf(a.out, "[%s] %s: %s\n", strings.ToUpper(check.Status), check.Name, check.Message)
	}
	fmt.Fprintln(a.out, "Overall:", report.Status)
	return nil
}

func (a *app) room(args []string, minimum, maximum int) (model.RoomCredential, []string, error) {
	if len(args) < minimum || len(args) > maximum {
		return model.RoomCredential{}, nil, fmt.Errorf("expected %d to %d arguments, got %d", minimum, maximum, len(args))
	}
	handle := ""
	if len(args) == maximum {
		handle, args = args[0], args[1:]
	}
	credential, err := a.deps.Config.LoadRoom(handle)
	if errors.Is(err, localconfig.ErrRoomAmbiguous) {
		return model.RoomCredential{}, nil, fmt.Errorf("%w; run 'agentline list'", err)
	}
	if errors.Is(err, localconfig.ErrRoomNotFound) && handle == "" {
		return model.RoomCredential{}, nil, fmt.Errorf("%w; run 'agentline create' or 'agentline join'", err)
	}
	return credential, args, err
}

func (a *app) advance(credential model.RoomCredential, messages []model.Message, cursor int64) error {
	sequence := cursor
	for _, message := range messages {
		if message.Sequence > sequence {
			sequence = message.Sequence
		}
	}
	return a.deps.Config.AdvanceCursor(credential.RoomID, sequence)
}

func (a *app) printJSON(value any) error { return json.NewEncoder(a.out).Encode(value) }
