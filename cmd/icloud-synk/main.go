// Command icloud-synk keeps a local folder in sync with iCloud Drive and
// downloads the iCloud photo library into it.
package main

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"runtime"
	"runtime/debug"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/kolesnikav/icloud-synk/internal/config"
	"github.com/kolesnikav/icloud-synk/internal/icloud"
	"github.com/kolesnikav/icloud-synk/internal/synk"
)

const usage = `icloud-synk — synchronise a local folder with iCloud

Usage:
  icloud-synk login   [--apple-id ID] [--dir PATH]   sign in (password + 2FA) and store the session
  icloud-synk sync    [--dry-run] [--drive] [--photos]   run one pass and exit
  icloud-synk daemon  [--poll 30s]                   keep syncing (inotify + polling)
  icloud-synk status                                 show session and state
  icloud-synk logout                                 forget the stored session
  icloud-synk config                                 print the effective configuration
  icloud-synk version                                print the build version

Common flags:
  --dir PATH        local folder to sync into (saved to the config)
  --verbose         debug logging
  --config-only     with login: only write the config, do not sign in

Configuration lives in ~/.config/icloud-synk/config.json, the session in
session.json next to it (mode 0600). The password is never stored.
`

// version is stamped at build time (see the Makefile). A binary downloaded from
// a release must be able to say which build it is.
var version = "dev"

func versionString() string {
	v := version
	if info, ok := debug.ReadBuildInfo(); ok {
		if v == "dev" && info.Main.Version != "" && info.Main.Version != "(devel)" {
			v = info.Main.Version
		}
	}
	return fmt.Sprintf("icloud-synk %s %s/%s %s", v, runtime.GOOS, runtime.GOARCH, runtime.Version())
}

func main() {
	if len(os.Args) < 2 {
		fmt.Fprint(os.Stderr, usage)
		os.Exit(2)
	}
	cmd := os.Args[1]
	args := os.Args[2:]

	var err error
	switch cmd {
	case "login":
		err = cmdLogin(args)
	case "sync":
		err = cmdSync(args)
	case "daemon":
		err = cmdDaemon(args)
	case "status":
		err = cmdStatus(args)
	case "logout":
		err = cmdLogout(args)
	case "config":
		err = cmdConfig(args)
	case "version", "--version", "-V":
		fmt.Println(versionString())
		return
	case "-h", "--help", "help":
		fmt.Print(usage)
		return
	default:
		fmt.Fprintf(os.Stderr, "unknown command %q\n\n%s", cmd, usage)
		os.Exit(2)
	}
	if err != nil {
		fmt.Fprintf(os.Stderr, "icloud-synk: %v\n", err)
		os.Exit(1)
	}
}

// ---------------------------------------------------------------- plumbing

func newLogger(verbose bool) *slog.Logger {
	level := slog.LevelInfo
	if verbose {
		level = slog.LevelDebug
	}
	// systemd captures stderr into the journal; keep the output plain.
	h := slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{
		Level: level,
		ReplaceAttr: func(groups []string, a slog.Attr) slog.Attr {
			if a.Key == slog.TimeKey {
				return slog.Attr{}
			}
			return a
		},
	})
	return slog.New(h)
}

func sessionPath() (string, error) {
	dir, err := config.Dir()
	if err != nil {
		return "", err
	}
	return filepath.Join(dir, "session.json"), nil
}

// loadConfig loads the config and applies the shared --dir override.
func loadConfig(dirOverride string) (*config.Config, error) {
	cfg, _, err := config.Load()
	if err != nil {
		return nil, err
	}
	if dirOverride != "" {
		cfg.TargetDir = dirOverride
	}
	if err := cfg.Validate(); err != nil {
		return nil, err
	}
	return cfg, nil
}

func newClient(log *slog.Logger) (*icloud.Client, error) {
	p, err := sessionPath()
	if err != nil {
		return nil, err
	}
	return icloud.New(p, icloud.WithLogger(func(f string, a ...any) {
		msg := fmt.Sprintf(f, a...)
		if strings.HasPrefix(msg, "iCloud upload response ") {
			log.Info(msg)
			return
		}
		log.Debug(msg)
	}))
}

func withSignals() (context.Context, context.CancelFunc) {
	return signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
}

// ------------------------------------------------------------------- login

func cmdLogin(args []string) error {
	fs := flag.NewFlagSet("login", flag.ExitOnError)
	appleID := fs.String("apple-id", "", "Apple ID (email)")
	dir := fs.String("dir", "", "local folder to sync into")
	noPhotos := fs.Bool("no-photos", false, "do not sync the photo library")
	configOnly := fs.Bool("config-only", false, "only write the configuration")
	verbose := fs.Bool("verbose", false, "debug logging")
	if err := fs.Parse(args); err != nil {
		return err
	}
	log := newLogger(*verbose)

	cfg, existed, err := config.Load()
	if err != nil {
		return err
	}
	if *dir != "" {
		cfg.TargetDir = *dir
	}
	if *appleID != "" {
		cfg.AppleID = *appleID
	}
	if *noPhotos {
		cfg.SyncPhotos = false
	}

	in := bufio.NewReader(os.Stdin)
	if cfg.AppleID == "" {
		cfg.AppleID, err = prompt(in, "Apple ID (email): ")
		if err != nil {
			return err
		}
	}
	if cfg.TargetDir == "" {
		def := filepath.Join(os.Getenv("HOME"), "iCloud")
		answer, err := prompt(in, fmt.Sprintf("Folder to sync into [%s]: ", def))
		if err != nil {
			return err
		}
		if strings.TrimSpace(answer) == "" {
			answer = def
		}
		cfg.TargetDir = answer
	}
	if err := cfg.Validate(); err != nil {
		return err
	}
	if err := cfg.Save(); err != nil {
		return err
	}
	if !existed {
		p, _ := config.Path()
		fmt.Printf("Wrote configuration to %s\n", p)
	}
	if err := os.MkdirAll(cfg.DriveDir(), 0o755); err != nil {
		return err
	}
	if *configOnly {
		return nil
	}

	cl, err := newClient(log)
	if err != nil {
		return err
	}

	password, err := readPassword(cfg)
	if err != nil {
		return err
	}

	ctx, cancel := withSignals()
	defer cancel()

	fmt.Println("Signing in to iCloud…")
	err = cl.Login(ctx, cfg.AppleID, password, func(ch *icloud.Challenge) (string, error) {
		return askTwoFactorCode(in, ch)
	})
	if err != nil {
		return err
	}

	fmt.Printf("Signed in as %s. Session stored.\n", cfg.AppleID)
	if exp, ok := cl.TrustExpiry(); ok {
		fmt.Printf("The session should stay valid until about %s.\n", exp.Format("2 Jan 2006"))
	}
	fmt.Printf("Target folder: %s\n", cfg.TargetDir)
	fmt.Println("Next: `icloud-synk sync` for a one-off pass, or `icloud-synk daemon` to keep syncing.")
	return nil
}

// askTwoFactorCode explains where the code can be found and lets the user
// switch delivery method. Apple never emails the code: it appears on a trusted
// device only after the "Allow" prompt is accepted, which is the usual reason
// people see nothing at all.
func askTwoFactorCode(in *bufio.Reader, ch *icloud.Challenge) (string, error) {
	length := 6
	if ch != nil && ch.CodeLength > 0 {
		length = ch.CodeLength
	}

	if ch != nil && ch.SecurityCode.TooManyCodesLock {
		return "", errors.New("iCloud has temporarily locked code requests for this account; " +
			"wait a few hours and try again")
	}

	fmt.Println()
	if ch == nil || ch.TrustedDeviceCount == 0 {
		fmt.Println("iCloud wants a two-factor code, but reports no trusted device to push to.")
	} else {
		fmt.Printf("iCloud pushed a sign-in prompt to %d trusted device(s).\n", ch.TrustedDeviceCount)
	}
	fmt.Printf("Where to find the %d-digit code:\n", length)
	fmt.Println("  • On your iPhone/iPad/Mac: tap \"Allow\" on the sign-in prompt — the code")
	fmt.Println("    is only shown after that.")
	fmt.Println("  • No prompt? Generate one on the device instead:")
	fmt.Println("    Settings → [your name] → Sign-In & Security → Two-Factor Authentication")
	fmt.Println("    → Get Verification Code.")
	if ch != nil && len(ch.PhoneNumbers) > 0 {
		fmt.Println("  • Or have it texted to a trusted number:")
		for _, p := range ch.PhoneNumbers {
			fmt.Printf("      sms %d   → %s\n", p.ID, p.Masked)
		}
	}
	fmt.Println("The code is never sent by email.")
	fmt.Println()
	fmt.Println("Commands: `sms [N]` request an SMS, `push` re-send to the devices, `q` cancel.")

	for {
		answer, err := prompt(in, "Two-factor code: ")
		if err != nil {
			return "", err
		}
		fields := strings.Fields(strings.ToLower(answer))
		switch {
		case len(fields) == 0:
			continue

		case fields[0] == "q" || fields[0] == "quit":
			return "", errors.New("sign-in cancelled")

		case fields[0] == "sms":
			if ch == nil || len(ch.PhoneNumbers) == 0 {
				fmt.Println("This account has no trusted phone number available.")
				continue
			}
			id := ch.PhoneNumbers[0].ID
			if len(fields) > 1 {
				n, convErr := strconv.Atoi(fields[1])
				if convErr != nil {
					fmt.Println("Usage: sms [number-id]")
					continue
				}
				id = n
			}
			if err := ch.SendSMS(id); err != nil {
				fmt.Printf("Could not request the SMS: %v\n", err)
				continue
			}
			fmt.Println("SMS requested — enter the code when it arrives.")

		case fields[0] == "push" || fields[0] == "resend":
			if err := ch.ResendPush(); err != nil {
				fmt.Printf("Could not re-send the prompt: %v\n", err)
				continue
			}
			fmt.Println("Prompt re-sent to your trusted devices.")

		default:
			// Accept the code with or without separators ("123 456").
			code := strings.Map(func(r rune) rune {
				if r >= '0' && r <= '9' {
					return r
				}
				return -1
			}, answer)
			if len(code) != length {
				fmt.Printf("A code has %d digits; got %d. Try again, or use `sms`/`push`.\n",
					length, len(code))
				continue
			}
			return code, nil
		}
	}
}

// readPassword takes the password from the environment, from a configured
// command, or interactively from the terminal.
func readPassword(cfg *config.Config) (string, error) {
	if v := os.Getenv("ICLOUD_PASSWORD"); v != "" {
		return v, nil
	}
	if cfg.PasswordCommand != "" {
		out, err := exec.Command("sh", "-c", cfg.PasswordCommand).Output()
		if err != nil {
			return "", fmt.Errorf("password_command failed: %w", err)
		}
		return strings.TrimRight(string(out), "\r\n"), nil
	}
	return promptPassword("Apple ID password: ")
}

func prompt(in *bufio.Reader, label string) (string, error) {
	fmt.Print(label)
	line, err := in.ReadString('\n')
	if err != nil && line == "" {
		return "", err
	}
	return strings.TrimSpace(line), nil
}

// promptPassword reads a line with terminal echo disabled. stty keeps this
// dependency-free; if it is unavailable the password would be echoed, so we
// refuse instead.
func promptPassword(label string) (string, error) {
	tty, err := os.OpenFile("/dev/tty", os.O_RDWR, 0)
	if err != nil {
		return "", fmt.Errorf("no terminal available for the password prompt; " +
			"set ICLOUD_PASSWORD or password_command in the config")
	}
	defer tty.Close()

	restore, err := disableEcho(tty)
	if err != nil {
		return "", err
	}
	defer restore()

	fmt.Fprint(tty, label)
	line, err := bufio.NewReader(tty).ReadString('\n')
	fmt.Fprintln(tty)
	if err != nil && line == "" {
		return "", err
	}
	pw := strings.TrimRight(line, "\r\n")
	if pw == "" {
		return "", errors.New("empty password")
	}
	return pw, nil
}

func disableEcho(tty *os.File) (restore func(), err error) {
	off := exec.Command("stty", "-echo")
	off.Stdin = tty
	if err := off.Run(); err != nil {
		return nil, fmt.Errorf("cannot disable terminal echo (stty): %w", err)
	}
	return func() {
		on := exec.Command("stty", "echo")
		on.Stdin = tty
		_ = on.Run()
	}, nil
}

// -------------------------------------------------------------------- sync

func cmdSync(args []string) error {
	fs := flag.NewFlagSet("sync", flag.ExitOnError)
	dir := fs.String("dir", "", "local folder to sync into")
	dryRun := fs.Bool("dry-run", false, "print what would happen, change nothing")
	onlyDrive := fs.Bool("drive", false, "sync only iCloud Drive")
	onlyPhotos := fs.Bool("photos", false, "sync only the photo library")
	verbose := fs.Bool("verbose", false, "debug logging")
	if err := fs.Parse(args); err != nil {
		return err
	}
	log := newLogger(*verbose)

	cfg, err := loadConfig(*dir)
	if err != nil {
		return err
	}
	if *onlyDrive {
		cfg.SyncPhotos = false
	}
	if *onlyPhotos {
		cfg.SyncDrive = false
	}

	cl, err := newClient(log)
	if err != nil {
		return err
	}
	ctx, cancel := withSignals()
	defer cancel()

	if err := cl.Authenticate(ctx); err != nil {
		return authHint(err)
	}

	st, err := synk.LoadState(cfg.StatePath())
	if err != nil {
		return err
	}
	eng := synk.NewEngine(cfg, cl, st, log, *dryRun)

	if cfg.SyncDrive {
		stats, err := eng.SyncDrive(ctx)
		if err != nil {
			return err
		}
		fmt.Printf("Drive: %s\n", stats.String())
	}
	if cfg.SyncPhotos {
		stats, err := eng.SyncPhotos(ctx)
		if err != nil {
			return err
		}
		fmt.Printf("Photos: %s\n", stats.String())
	}
	return finishSync(st, *dryRun)
}

// finishSync leaves the state untouched during a dry run.
func finishSync(st *synk.State, dryRun bool) error {
	if dryRun {
		return nil
	}
	st.MarkSynced(time.Now())
	return st.Save()
}

// ------------------------------------------------------------------ daemon

func cmdDaemon(args []string) error {
	fs := flag.NewFlagSet("daemon", flag.ExitOnError)
	dir := fs.String("dir", "", "local folder to sync into")
	poll := fs.Duration("poll", 0, "how often to poll iCloud for changes")
	verbose := fs.Bool("verbose", false, "debug logging")
	if err := fs.Parse(args); err != nil {
		return err
	}
	log := newLogger(*verbose)

	cfg, err := loadConfig(*dir)
	if err != nil {
		return err
	}
	if *poll > 0 {
		cfg.PollInterval = config.Duration(*poll)
	}

	cl, err := newClient(log)
	if err != nil {
		return err
	}
	ctx, cancel := withSignals()
	defer cancel()

	// A dead session is not fatal here: the daemon keeps running and notifies
	// the user, then picks up the new session once they log in.
	if err := cl.Authenticate(ctx); err != nil {
		if !errors.Is(err, icloud.ErrLoginRequired) {
			return err
		}
		log.Error("no usable iCloud session — run `icloud-synk login`")
	}

	st, err := synk.LoadState(cfg.StatePath())
	if err != nil {
		return err
	}
	eng := synk.NewEngine(cfg, cl, st, log, false)
	d := synk.NewDaemon(eng, cl, log)
	if err := d.Run(ctx); err != nil {
		return err
	}
	return st.Save()
}

// ------------------------------------------------------------------ status

// daemonSyncStatus reads recent events from the currently running user service.
func daemonSyncStatus() string {
	pidOut, err := exec.Command(
		"systemctl", "--user", "show", "icloud-synk.service",
		"-p", "MainPID", "--value",
	).Output()
	if err != nil {
		return "Unbekannt"
	}

	pid := strings.TrimSpace(string(pidOut))
	if pid == "" || pid == "0" {
		return "Unbekannt"
	}

	out, err := exec.Command(
		"journalctl", "--user", "-u", "icloud-synk.service",
		"_PID="+pid, "--since=5 minutes ago", "-n", "50", "--no-pager", "-o", "short-iso",
	).Output()
	if err != nil {
		return "Unbekannt"
	}

	return syncStatusFromJournal(string(out))
}

func syncStatusFromJournal(journal string) string {
	status := "Unbekannt"
	var latestEvent time.Time
	var lastStart time.Time

	for _, line := range strings.Split(journal, "\n") {
		fields := strings.Fields(line)
		if len(fields) < 2 {
			continue
		}

		timestamp, err := time.Parse(time.RFC3339, fields[0])
		if err != nil {
			continue
		}

		messageStatus := ""
		switch {
		case strings.Contains(line, `msg="drive pass started"`):
			messageStatus = "Läuft"
		case strings.Contains(line, `msg="drive pass completed"`):
			messageStatus = "Wartet auf nächsten Durchlauf"
		case strings.Contains(line, `msg="drive pass failed"`):
			messageStatus = "Letzter Durchlauf fehlgeschlagen"
		case strings.Contains(line, `msg="drive pass skipped"`):
			messageStatus = "Durchlauf übersprungen"
		default:
			continue
		}

		if timestamp.Before(latestEvent) {
			continue
		}

		latestEvent = timestamp
		status = messageStatus
		if messageStatus == "Läuft" {
			lastStart = timestamp
		}
	}

	if status == "Läuft" {
		now := time.Now()
		if lastStart.IsZero() || lastStart.After(now) || now.Sub(lastStart) > 2*time.Minute {
			return "Unbekannt"
		}
	}

	return status
}

func cmdStatus(args []string) error {
	fs := flag.NewFlagSet("status", flag.ExitOnError)
	check := fs.Bool("check", false, "verify the iCloud session online")
	if err := fs.Parse(args); err != nil {
		return err
	}
	log := newLogger(false)

	cfg, _, err := config.Load()
	if err != nil {
		return err
	}
	cl, err := newClient(log)
	if err != nil {
		return err
	}
	info := cl.SessionInfo()

	fmt.Printf("Apple ID:      %s\n", orDash(info.AppleID))
	fmt.Printf("Target folder: %s\n", orDash(cfg.TargetDir))
	fmt.Printf("Sync:          drive=%v photos=%v\n", cfg.SyncDrive, cfg.SyncPhotos)
	if out, err := exec.Command("systemctl", "--user", "is-active", "icloud-synk.service").CombinedOutput(); err == nil {
		fmt.Printf("Daemon:        %s\n", strings.TrimSpace(string(out)))
	} else {
		state := strings.TrimSpace(string(out))
		if state == "" {
			state = "unknown"
		}
		fmt.Printf("Daemon:        %s\n", state)
	}
	fmt.Printf("Sync status:   %s\n", daemonSyncStatus())
	fmt.Printf("dsid:          %s\n", orDash(info.DSID))
	if len(info.Webservices) > 0 {
		names := make([]string, 0, len(info.Webservices))
		for n := range info.Webservices {
			names = append(names, n)
		}
		fmt.Printf("Services:      %d available\n", len(names))
	}
	if info.TrustToken == "" {
		fmt.Println("Session:       none — run `icloud-synk login`")
	} else if exp, ok := cl.TrustExpiry(); ok {
		left := time.Until(exp)
		fmt.Printf("Session:       trusted, ~%d days left\n", int(left.Hours()/24))
	} else {
		fmt.Println("Session:       trusted")
	}

	if cfg.TargetDir != "" {
		if err := cfg.Validate(); err == nil {
			if st, err := synk.LoadState(cfg.StatePath()); err == nil {
				drive, photos := st.Counts()
				fmt.Printf("Tracked:       %d Drive entries, %d photos\n", drive, photos)
				if t := st.SyncedAt(); !t.IsZero() {
					fmt.Printf("Last pass:     %s\n", t.Format(time.RFC1123))
				}
			}
		}
	}

	if !*check {
		fmt.Println("Check:         skipped (local status; use --check for online verification)")
		return nil
	}

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	if err := cl.Authenticate(ctx); err != nil {
		fmt.Printf("Check:         FAILED — %v\n", err)
		return nil
	}
	fmt.Println("Check:         session works")
	return nil
}

func cmdLogout(args []string) error {
	log := newLogger(false)
	cl, err := newClient(log)
	if err != nil {
		return err
	}
	if err := cl.Logout(); err != nil {
		return err
	}
	fmt.Println("Session removed. The local folder and its state were left untouched.")
	return nil
}

func cmdConfig(args []string) error {
	cfg, existed, err := config.Load()
	if err != nil {
		return err
	}
	p, _ := config.Path()
	if !existed {
		fmt.Printf("// %s does not exist yet; showing defaults\n", p)
	}
	_ = cfg.Validate() // normalise paths for display; an error is fine here
	enc, err := json.MarshalIndent(cfg, "", "  ")
	if err != nil {
		return err
	}
	fmt.Println(string(enc))
	return nil
}

// authHint turns a login failure into an actionable message.
func authHint(err error) error {
	if errors.Is(err, icloud.ErrLoginRequired) {
		return fmt.Errorf("not signed in (or the session expired) — run `icloud-synk login`")
	}
	return err
}

func orDash(s string) string {
	if s == "" {
		return "—"
	}
	return s
}
