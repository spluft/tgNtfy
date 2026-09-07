// Package admin implements the CLI subcommands of the tgntfy binary (A-3): service
// create/list/enable/disable/rotate, link/unlink, user list, events recent. It runs
// against the same DB via DB_PATH and never logs/persists raw tokens.
package admin

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"flag"
	"fmt"
	"io"
	"os"
	"strconv"
	"strings"

	"github.com/spluft/tgNtfy/internal/store"
)

// Run executes an admin subcommand. DB path is passed from the command line.
func Run(args []string, stdout, stderr io.Writer) {
	if len(args) < 2 {
		fmt.Fprintln(stderr, "usage: tgntfy admin <db-path> <subcommand> [args]")
		os.Exit(2)
	}
	dbPath, sub := args[0], args[1]
	ctx := context.Background()
	st, err := store.New(dbPath)
	if err != nil {
		fmt.Fprintf(stderr, "open db: %v\n", err)
		os.Exit(1)
	}
	defer st.Close()

	switch sub {
	case "service":
		svcArgs(args[2:], st, stdout, stderr)
	case "link":
		if len(args) != 5 {
			fmt.Fprintln(stderr, "usage: tgntfy admin <db> link <service> <user_ref> <tg_user_id>")
			os.Exit(2)
		}
		uid, err := strconv.ParseInt(args[4], 10, 64)
		if err != nil {
			fmt.Fprintf(stderr, "bad tg_user_id: %v\n", err)
			os.Exit(2)
		}
		if err := st.AdminLink(ctx, args[2], args[3], uid); err != nil {
			fmt.Fprintf(stderr, "link failed: %v\n", err)
			os.Exit(1)
		}
		fmt.Fprintf(stdout, "linked %s user_ref=%s -> tg_user=%d\n", args[2], args[3], uid)
	case "unlink":
		if len(args) != 4 {
			fmt.Fprintln(stderr, "usage: tgntfy admin <db> unlink <service> <user_ref>")
			os.Exit(2)
		}
		if err := st.AdminUnlink(ctx, args[2], args[3]); err != nil {
			fmt.Fprintf(stderr, "unlink failed: %v\n", err)
			os.Exit(1)
		}
		fmt.Fprintf(stdout, "unlinked %s user_ref=%s\n", args[2], args[3])
	case "user":
		listUsers(ctx, st, stdout, stderr)
	case "events":
		eventsRecent(ctx, args[2:], st, stdout, stderr)
	default:
		fmt.Fprintf(stderr, "unknown admin subcommand %q\n", sub)
		os.Exit(2)
	}
}

func svcArgs(args []string, st *store.Store, stdout, stderr io.Writer) {
	ctx := context.Background()
	if len(args) == 0 {
		fmt.Fprintln(stderr, "usage: tgntfy admin <db> service <create|list|enable|disable|rotate> ...")
		os.Exit(2)
	}
	cmd := args[0]
	fs := flag.NewFlagSet("service", flag.ExitOnError)
	var name string
	rest := make([]string, 0, len(args[1:]))
	rest = append(rest, args[1:]...)
	// Go's flag stops at the first positional; the SPEC syntax is
	// `service create <id> --name <display>`, so pull --name out explicitly.
	for i := 0; i < len(rest); i++ {
		if rest[i] == "--name" && i+1 < len(rest) {
			name = rest[i+1]
			copy(rest[i:], rest[i+1:])
			rest = rest[:len(rest)-1]
			break
		}
	}
	_ = fs.Parse(rest)
	switch cmd {
	case "list":
		list, err := st.ServiceList(ctx)
		if err != nil {
			fmt.Fprintf(stderr, "list: %v\n", err)
			os.Exit(1)
		}
		for _, s := range list {
			state := "disabled"
			if s.Enabled == 1 {
				state = "enabled"
			}
			fmt.Fprintf(stdout, "%-16s %-16s %s\n", s.Service, s.DisplayName, state)
		}
	case "create":
		if fs.NArg() < 1 {
			fmt.Fprintln(stderr, "usage: service create <id> --name <display>")
			os.Exit(2)
		}
		id := fs.Arg(0)
		if name == "" {
			fmt.Fprintln(stderr, "--name required")
			os.Exit(2)
		}
		tok := newToken()
		if err := st.CreateService(ctx, id, name, tok); err != nil {
			fmt.Fprintf(stderr, "create: %v\n", err)
			os.Exit(1)
		}
		fmt.Fprintf(stdout, "created service %q (display %q)\nRAW TOKEN (print once):\n%s\n", id, name, tok)
	case "rotate":
		if fs.NArg() < 1 {
			fmt.Fprintln(stderr, "usage: service rotate <id>")
			os.Exit(2)
		}
		id := fs.Arg(0)
		tok := newToken()
		if err := st.RotateServiceToken(ctx, id, tok); err != nil {
			fmt.Fprintf(stderr, "rotate: %v\n", err)
			os.Exit(1)
		}
		fmt.Fprintf(stdout, "rotated %q\nRAW TOKEN (print once):\n%s\n", id, tok)
	case "enable":
		needArgs(fs, 1, "enable <id>", stderr)
		if err := st.SetEnabled(ctx, fs.Arg(0), true); err != nil {
			fmt.Fprintf(stderr, "enable: %v\n", err)
			os.Exit(1)
		}
		fmt.Fprintf(stdout, "enabled %s\n", fs.Arg(0))
	case "disable":
		needArgs(fs, 1, "disable <id>", stderr)
		if err := st.SetEnabled(ctx, fs.Arg(0), false); err != nil {
			fmt.Fprintf(stderr, "disable: %v\n", err)
			os.Exit(1)
		}
		fmt.Fprintf(stdout, "disabled %s\n", fs.Arg(0))
	default:
		fmt.Fprintf(stderr, "unknown service subcommand %q\n", cmd)
		os.Exit(2)
	}
}

func listUsers(ctx context.Context, st *store.Store, stdout, stderr io.Writer) {
	list, err := st.UserList(ctx)
	if err != nil {
		fmt.Fprintf(stderr, "users: %v\n", err)
		os.Exit(1)
	}
	for _, u := range list {
		grp := "-"
		if u.GroupChatID != nil {
			grp = strconv.FormatInt(*u.GroupChatID, 10)
		}
		fmt.Fprintf(stdout, "user=%d mode=%s group=%s services=%s\n", u.TgUserID, u.DeliveryMode, grp, strings.Join(st.LinkedServices(ctx, u.TgUserID), ","))
	}
}

func eventsRecent(ctx context.Context, args []string, st *store.Store, stdout, stderr io.Writer) {
	n := 20
	if len(args) >= 2 && args[0] == "-n" {
		if v, err := strconv.Atoi(args[1]); err == nil {
			n = v
		}
	}
	list, err := st.RecentEvents(ctx, n)
	if err != nil {
		fmt.Fprintf(stderr, "events: %v\n", err)
		os.Exit(1)
	}
	for _, e := range list {
		fmt.Fprintf(stdout, "%s [%s/%s] %s | %s\n", e.Received.Format("2006-01-02 15:04:05"), e.Service, e.Type, e.UserRef, e.Title)
	}
}

func needArgs(fs *flag.FlagSet, n int, usage string, stderr io.Writer) {
	if fs.NArg() < n {
		fmt.Fprintf(stderr, "usage: service %s\n", usage)
		os.Exit(2)
	}
}

// newToken returns 32 random bytes hex-encoded (64 chars).
func newToken() string {
	b := make([]byte, 32)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}
