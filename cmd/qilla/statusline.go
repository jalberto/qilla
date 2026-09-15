package main

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"sync"
	"time"

	"github.com/jalberto/qilla/internal/config"
	"github.com/jalberto/qilla/internal/doctor"
	"github.com/jalberto/qilla/internal/install"
	"github.com/jalberto/qilla/internal/models"
	"github.com/jalberto/qilla/internal/statusline"
)

// cmdStatusline: qilla statusline — the user-level status line command, fed the
// session JSON on stdin. With [chat].statusline set it wraps the user's own
// status line (badged in a qilla session); empty — the default — renders
// qilla's native bar: model, weekly usage, context, failing services, reminders
// due, open questions, inbox jots.
func cmdStatusline(args []string) error {
	raw, _ := io.ReadAll(os.Stdin)
	cfg, err := config.Load(config.DefaultPath())
	if err == nil && cfg.Chat.StatusLine != "" {
		cmd := cfg.Chat.StatusLine
		if os.Getenv("QILLA_RUN") != "" {
			cmd = install.StatusLineCommand(filepath.Dir(cfg.Path), cfg.Chat.StatusLine)
		}
		c := exec.Command("sh", "-c", cmd)
		c.Stdin, c.Stdout, c.Stderr = bytes.NewReader(raw), os.Stdout, os.Stderr
		return c.Run()
	}
	fmt.Println(statusline.Render(statuslineView(cfg, raw)))
	return nil
}

func statuslineView(cfg *config.Config, raw []byte) statusline.View {
	in := statusline.Parse(bytes.NewReader(raw))
	v := statusline.View{
		Qilla:   os.Getenv("QILLA_RUN") != "",
		Who:     statuslineWho(),
		Model:   in.Model.DisplayName,
		Usage:   statusline.Pct(in.RateLimits.SevenDay.UsedPercentage),
		Context: statusline.Pct(in.ContextWindow.UsedPercentage),
	}
	if cfg == nil {
		return v
	}
	if u := models.Usage(cfg.Models.UsageCache, time.Now()); u >= 0 {
		v.Usage = u
	}
	var wg sync.WaitGroup
	wg.Add(2)
	go func() { defer wg.Done(); v.Services = failingServices(cfg) }()
	go func() { defer wg.Done(); v.Reminders = remindersDue() }()
	v.Questions = statusline.CountOpenTasks(cfg.VaultPath(cfg.Questions))
	v.Jots = statusline.CountInboxJots(cfg.VaultPath(filepath.Join(cfg.JournalDir, time.Now().Format("2006-01-02")+".md")))
	wg.Wait()
	return v
}

func statuslineWho() string {
	for _, k := range []string{"QILLA_ROUTINE", "QILLA_AGENT"} {
		if v := os.Getenv(k); v != "" {
			return v
		}
	}
	return "chief"
}

// failingServices counts hard doctor failures, probing only what is cheap: no
// claude, mise, systemd or engram calls, since the bar must render in well
// under 150 ms. Checks that would misreport without their probe are dropped.
func failingServices(cfg *config.Config) int {
	home, _ := os.UserHomeDir()
	env := doctor.Env{
		LookPath:  exec.LookPath,
		Stat:      os.Stat,
		Home:      home,
		ConfigDir: filepath.Dir(cfg.Path),
		Run: func(name string, args ...string) (string, error) {
			if filepath.Base(name) != "git" {
				return "", errors.New("skipped: statusline runs only cheap checks")
			}
			out, err := exec.Command(name, args...).Output()
			return string(out), err
		},
	}
	n := 0
	for _, c := range doctor.Run(cfg, nil, env) {
		if c.Hard && !c.OK && c.Name != "claude login" {
			n++
		}
	}
	return n
}

// remindersDue shells out to `qilla remind count` — a separate command with its
// own state; a slow or missing one costs the bar nothing.
func remindersDue() int {
	self, err := os.Executable()
	if err != nil {
		return 0
	}
	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()
	out, err := exec.CommandContext(ctx, self, "remind", "count").Output()
	if err != nil {
		return 0
	}
	return statusline.Count(string(out))
}
