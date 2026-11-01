package pando

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"time"
)

func claudeRoot(env CLIEnv, f cliFlags) string {
	if f.claudeDir != "" {
		return f.claudeDir
	}
	return filepath.Join(env.Home, ".claude", "projects")
}

func hermesSourceOf(env CLIEnv, f cliFlags) (Source, error) {
	if env.HermesSource == nil {
		return nil, fmt.Errorf("this build cannot read Hermes chats")
	}
	return env.HermesSource(f.hermesProfile)
}

// cliImportHermes is import --from hermes: log every Hermes chat of the
// chosen profiles now. The act id keeps a message once, so it can run again.
func cliImportHermes(env CLIEnv, dir string, f cliFlags) int {
	src, err := hermesSourceOf(env, f)
	if err != nil {
		fmt.Fprintln(env.Err, err)
		return 1
	}
	if f.dry {
		fmt.Fprintln(env.Out, "hermes: would read the chats of", src.Name())
		fmt.Fprintln(env.Out, "nothing was changed")
		return 0
	}
	ctx := context.Background()
	c, err := OpenWait(ctx, Config{Dir: dir, Now: env.Now}, 20*time.Second)
	if err != nil {
		fmt.Fprintln(env.Err, err)
		return 1
	}
	defer c.Close()
	n, err := src.Poll(ctx, c, time.Time{})
	fmt.Fprintf(env.Out, "hermes: %d messages logged\n", n)
	if err != nil {
		fmt.Fprintln(env.Err, err)
		return 1
	}
	return 0
}

// cliFollow is `daisugi pando follow`.
func cliFollow(env CLIEnv, dir string, f cliFlags) int {
	profile := f.profile
	if profile == "" {
		profile = ReadProfile(dataOf(env, f), env.Getenv)
	}
	o := &FollowOpts{Dir: dir, Now: env.Now, Log: env.Err, Interval: time.Duration(f.interval) * time.Second}
	o.Sources = append(o.Sources, &ClaudeSource{Root: claudeRoot(env, f), Skip: SkipDirs(dir, env.Home, env.Getenv)})
	if env.HermesSource != nil {
		src, err := env.HermesSource(f.hermesProfile)
		if err != nil {
			fmt.Fprintln(env.Err, "follow: hermes:", err)
		} else {
			o.Sources = append(o.Sources, src)
		}
	}
	if profile != "" && profile != "default" && env.NewSummarizer != nil {
		s, err := env.NewSummarizer(profile)
		if err != nil {
			fmt.Fprintln(env.Err, err)
			return 1
		}
		o.Summarizer = s
	}
	if f.once {
		added, built, more, err := FollowOnce(context.Background(), o)
		if err != nil {
			fmt.Fprintln(env.Err, err)
			return 1
		}
		fmt.Fprintf(env.Out, "logged: %d, built a node: %v, more due: %v\n", added, built, more)
		return 0
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		fmt.Fprintln(env.Err, err)
		return 1
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	fmt.Fprintf(env.Err, "follow: store %s, %d sources\n", dir, len(o.Sources))
	if err := Follow(ctx, o); err != nil {
		fmt.Fprintln(env.Err, err)
		return 1
	}
	return 0
}

// ProfileFile is where the background profile is kept, under <data>/pando.
const ProfileFile = "profile"

// ReadProfile is the agent profile that builds nodes in the background: the
// DAISUGI_PANDO_PROFILE variable (getenv), else the profile file. "" means
// none.
func ReadProfile(dataDir string, getenv func(string) string) string {
	if getenv != nil {
		if v := strings.TrimSpace(getenv("DAISUGI_PANDO_PROFILE")); v != "" {
			return v
		}
	}
	b, err := os.ReadFile(filepath.Join(dataDir, "pando", ProfileFile))
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(b))
}

func cliProfile(env CLIEnv, data string, args []string) int {
	path := filepath.Join(data, "pando", ProfileFile)
	if len(args) == 0 {
		if p := ReadProfile(data, nil); p != "" {
			fmt.Fprintln(env.Out, p)
		} else {
			fmt.Fprintln(env.Out, "none: nodes wait for a profile")
		}
		return 0
	}
	if args[0] == "none" {
		if err := os.Remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
			fmt.Fprintln(env.Err, err)
			return 1
		}
		fmt.Fprintln(env.Out, "no profile: nodes wait")
		return 0
	}
	if strings.ContainsAny(args[0], " \t\n/\\") {
		return usageErr(env, "a profile name has no spaces or slashes, not %q", args[0])
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		fmt.Fprintln(env.Err, err)
		return 1
	}
	if err := os.WriteFile(path, []byte(args[0]+"\n"), 0o600); err != nil {
		fmt.Fprintln(env.Err, err)
		return 1
	}
	fmt.Fprintln(env.Out, "profile:", args[0])
	return 0
}

func dataOf(env CLIEnv, f cliFlags) string {
	if f.dataDir != "" {
		return f.dataDir
	}
	return env.DataHome
}
