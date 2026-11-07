package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"strings"
	"time"

	"github.com/noahkiss/flightdeck/api"
)

// capcomServiceCmd is `gff-hab capcom say|thread|stop|pause|resume|status`
// (capcom-service.md 7.7, 11): the Capcom service from the command line.
func capcomServiceCmd(args []string, in io.Reader, out io.Writer) error {
	verb := args[0]
	fs := flag.NewFlagSet("capcom "+verb, flag.ExitOnError)
	sock := socketFlag(fs)
	via := fs.String("via", "", "a command whose stdin and stdout reach a hab, such as: ssh host gff-hab connect")
	follow := fs.Bool("follow", false, "thread: keep printing new items")
	limit := fs.Int("limit", 50, "thread: print at most this many of the newest items")
	fs.Parse(args[1:])
	if verb != "say" && fs.NArg() != 0 {
		return fmt.Errorf("capcom %s takes no arguments", verb)
	}
	text := strings.Join(fs.Args(), " ")
	if verb == "say" {
		if text == "-" {
			b, err := io.ReadAll(io.LimitReader(in, 1<<20))
			if err != nil {
				return err
			}
			text = strings.TrimRight(string(b), "\n")
		}
		if strings.TrimSpace(text) == "" {
			return errors.New("capcom say needs text, or - to read it from stdin")
		}
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	c, err := open(*sock, *via)
	if err != nil {
		return err
	}
	defer c.Close()
	hctx, hcancel := context.WithTimeout(ctx, 30*time.Second)
	defer hcancel()
	if _, err := c.Hello(hctx, cliClient, "", ""); err != nil {
		return err
	}
	cl := api.Client{Caller: c}
	switch verb {
	case "say":
		st, err := cl.CapcomStatus(hctx, api.CapcomServiceParams{})
		if err != nil {
			return err
		}
		if st.Session == "" {
			return errors.New("this hab does not run the Capcom service now")
		}
		if _, err := cl.SessionSend(hctx, api.SessionSendParams{Session: st.Session, Text: text, Origin: api.SendOriginTyped}); err != nil {
			return err
		}
		fmt.Fprintln(out, "delivered")
		return nil
	case "stop":
		if _, err := cl.CapcomInterrupt(hctx, api.CapcomServiceParams{}); err != nil {
			return err
		}
		fmt.Fprintln(out, "stopping the current turn")
		return nil
	case "pause", "resume", "status":
		var st api.CapcomServiceStatus
		switch verb {
		case "pause":
			st, err = cl.CapcomPause(hctx, api.CapcomServiceParams{})
		case "resume":
			st, err = cl.CapcomResume(hctx, api.CapcomServiceParams{})
		default:
			st, err = cl.CapcomStatus(hctx, api.CapcomServiceParams{})
		}
		if err != nil {
			return err
		}
		printServiceStatus(out, st)
		return nil
	}
	page, err := cl.CapcomThreadPage(hctx, api.CapcomThreadPageParams{Limit: *limit})
	if err != nil {
		return err
	}
	last := ""
	for i := len(page.Items) - 1; i >= 0; i-- {
		printItem(out, page.Items[i])
		last = page.Items[i].ID
	}
	for *follow {
		time.Sleep(time.Second)
		pctx, pcancel := context.WithTimeout(ctx, 30*time.Second)
		page, err := cl.CapcomThreadPage(pctx, api.CapcomThreadPageParams{})
		pcancel()
		if err != nil {
			return err
		}
		for i := len(page.Items) - 1; i >= 0; i-- {
			if page.Items[i].ID > last {
				printItem(out, page.Items[i])
				last = page.Items[i].ID
			}
		}
	}
	return nil
}

func printServiceStatus(out io.Writer, st api.CapcomServiceStatus) {
	fmt.Fprintf(out, "state %s", st.State)
	if st.Reason != "" {
		fmt.Fprintf(out, ": %s", st.Reason)
	}
	fmt.Fprintln(out)
	fmt.Fprintf(out, "session %s\nconnected %v\nfolder %s\n", st.Session, st.Connected, st.Folder)
	if st.Turn != "" {
		fmt.Fprintf(out, "turn %s\n", st.Turn)
	}
	for _, s := range st.SDKSessions {
		end := "current"
		if s.End != "" {
			end = "ended " + s.End
		}
		fmt.Fprintf(out, "sdk session %s (%s, %s)\n", s.ID, s.Why, end)
	}
	if st.State == "paused" {
		fmt.Fprintf(out, "resume it in a terminal: cd %s && claude --resume <id>; then gff-hab capcom resume\n", st.Folder)
	}
}

// printItem prints one thread item as one line (7.7).
func printItem(out io.Writer, it api.CapcomItem) {
	t := it.Time
	if len(t) >= 19 {
		t = t[11:19]
	}
	line := func(who, text string) {
		fmt.Fprintf(out, "%s %s: %s\n", t, who, strings.ReplaceAll(text, "\n", "\n    "))
	}
	switch it.Kind {
	case "message":
		who := "capcom"
		if it.Author.Kind == "user" {
			who = "you"
			if it.Read == "" {
				who += " (delivered)"
			}
		}
		line(who, it.Text)
	case "tools":
		var parts []string
		for _, c := range it.Tools {
			parts = append(parts, c.Summary)
		}
		line("tools", strings.Join(parts, ", "))
	case "card":
		state := "open"
		if it.Answer != nil {
			state = it.Answer.Choice
		}
		ref := ""
		if it.Card != nil {
			ref = it.Card.Type + " " + it.Card.Ref
		}
		line("card", fmt.Sprintf("%s [%s] %s", ref, state, it.Text))
	case "agent":
		line("report", it.Text)
	default:
		line(string(it.Kind), it.Text)
	}
}
