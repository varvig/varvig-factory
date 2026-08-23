// Command varvig-factory is the Factory cell binary.
//
// One static binary per platform, same discipline as varvig (FACTORY.md §1.2,
// varvig-design.md §3). Micro, Mini and Medium are configuration profiles this
// binary reads — not builds of it, and not code paths inside it.
package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/varvig/varvig-factory/agreement"
	"github.com/varvig/varvig-factory/cell"
	"github.com/varvig/varvig-factory/gate"
	"github.com/varvig/varvig-factory/profile"
	"github.com/varvig/varvig-factory/promote"
)

// Version is set at build time with -ldflags "-X main.Version=…".
var Version = "dev"

const defaultConfig = "factory.json"

func main() {
	args := os.Args[1:]
	if len(args) == 0 {
		usage()
		os.Exit(2)
	}
	cmd, rest := args[0], args[1:]
	if cmd == "-h" || cmd == "--help" || cmd == "help" {
		usage()
		return
	}
	if cmd == "-v" || cmd == "--version" {
		cmd = "version"
	}

	handlers := map[string]func([]string) error{
		"init":         cmdInit,
		"capabilities": cmdCapabilities,
		"once":         cmdOnce,
		"run":          cmdRun,
		"verify":       cmdVerify,
		"promote":      cmdPromote,
		"agreement":    cmdAgreement,
		"budget":       cmdBudget,
		"gate":         cmdGate,
		"version":      cmdVersion,
	}
	h, ok := handlers[cmd]
	if !ok {
		usage()
		fmt.Fprintf(os.Stderr, "\nunknown command %q\n", cmd)
		os.Exit(2)
	}
	if err := h(rest); err != nil {
		fmt.Fprintf(os.Stderr, "varvig-factory %s: %v\n", cmd, err)
		os.Exit(1)
	}
}

func usage() {
	fmt.Fprint(os.Stderr, `varvig-factory — an autonomous cell that turns tickets into verified,
promotable changes on top of varvig.

  varvig-factory init --profile micro|mini|medium --cell-id ID [--upstream ADDR]
                                    write a starting config (default: `+defaultConfig+`)
  varvig-factory capabilities [-c F] print this cell's capabilities and publish them
  varvig-factory once [-c F]         run one pass of the cell loop and report
  varvig-factory run [-c F]          loop until interrupted
  varvig-factory verify [-c F]       verify peer attempts only (the Micro cell's job)

  varvig-factory promote --mode gated|autonomous [-c F]
                                    set the live promotion mode; takes effect
                                    immediately, without a restart (§6.5)
  varvig-factory promote --enable PATH [-c F]
                                    enable autonomous promotion for one path scope
  varvig-factory promote --disable PATH [-c F]
  varvig-factory promote --status [-c F]

  varvig-factory agreement [-c F] [--scope PATH]
                                    report the promotion-agreement rate per scope
  varvig-factory budget [-c F]      report today's spend against the declared caps
  varvig-factory gate --bind MODULE.wasm [-c F]
                                    bind the promotion-policy wasm module (§6.2)
  varvig-factory version

Promotion is gated by default, everywhere. Autonomous mode is per-path, requires
a measured agreement rate for that path, and requires evidence from a cell other
than the one that authored the attempt. See CELL.md and the README.
`)
}

// flags is a tiny flag reader. The standard flag package would do, but every
// command here takes the same -c and a couple of string options, and a hand-rolled
// reader keeps the "unknown flag" error naming the command rather than the
// package.
type flags struct {
	config string
	values map[string]string
	bools  map[string]bool
}

func parseFlags(args []string, stringFlags, boolFlags []string) (flags, error) {
	f := flags{config: defaultConfig, values: map[string]string{}, bools: map[string]bool{}}
	known := map[string]bool{}
	for _, s := range stringFlags {
		known[s] = true
	}
	boolKnown := map[string]bool{}
	for _, b := range boolFlags {
		boolKnown[b] = true
	}
	for i := 0; i < len(args); i++ {
		a := args[i]
		switch {
		case a == "-c" || a == "--config":
			if i+1 >= len(args) {
				return f, fmt.Errorf("%s requires a value", a)
			}
			f.config, i = args[i+1], i+1
		case strings.HasPrefix(a, "--"):
			name, value, inline := strings.Cut(strings.TrimPrefix(a, "--"), "=")
			switch {
			case boolKnown[name]:
				f.bools[name] = true
			case known[name]:
				if inline {
					f.values[name] = value
					continue
				}
				if i+1 >= len(args) {
					return f, fmt.Errorf("--%s requires a value", name)
				}
				f.values[name], i = args[i+1], i+1
			default:
				return f, fmt.Errorf("unknown flag --%s", name)
			}
		default:
			return f, fmt.Errorf("unexpected argument %q", a)
		}
	}
	return f, nil
}

func cmdVersion([]string) error {
	fmt.Printf("varvig-factory %s\n", Version)
	return nil
}

func cmdInit(args []string) error {
	f, err := parseFlags(args, []string{"profile", "cell-id", "upstream"}, []string{"force"})
	if err != nil {
		return err
	}
	name := f.values["profile"]
	if name == "" {
		name = "micro"
	}
	cellID := f.values["cell-id"]
	if cellID == "" {
		return errors.New("--cell-id is required; a cell id appears in every ref this cell writes and is chosen once")
	}
	if err := cell.CheckID(cellID); err != nil {
		return err
	}
	cfg, err := profile.Template(name, cellID, f.values["upstream"])
	if err != nil {
		return err
	}
	if _, err := os.Stat(f.config); err == nil && !f.bools["force"] {
		return fmt.Errorf("%s already exists; pass --force to overwrite", f.config)
	}
	if err := profile.Save(f.config, cfg); err != nil {
		return err
	}
	fmt.Printf("wrote %s (profile %s, cell %s)\n", f.config, name, cellID)
	fmt.Printf("roles: %s\n", rolesOf(cfg))
	if !cfg.Capabilities().Has(cell.RoleAttempt) {
		fmt.Println("this cell verifies and builds but does not attempt; attempting is opt-in (§3.1)")
	}
	return nil
}

func rolesOf(cfg profile.Config) string {
	caps := cfg.Capabilities()
	out := make([]string, 0, len(caps.Roles))
	for _, r := range caps.Roles {
		out = append(out, string(r))
	}
	return strings.Join(out, " ")
}

// load reads the config and wires a cell.
func load(f flags) (profile.Config, profile.Built, error) {
	cfg, err := profile.Load(f.config)
	if err != nil {
		return cfg, profile.Built{}, err
	}
	built, err := cfg.Wire(nil)
	return cfg, built, err
}

func cmdCapabilities(args []string) error {
	f, err := parseFlags(args, nil, []string{"no-publish"})
	if err != nil {
		return err
	}
	cfg, built, err := load(f)
	if err != nil {
		return err
	}
	caps := cfg.Capabilities()
	raw, err := cell.Canonical(caps)
	if err != nil {
		return err
	}
	hash, err := caps.Hash()
	if err != nil {
		return err
	}
	fmt.Println(string(raw))
	fmt.Printf("hash %s\n", hash)
	if f.bools["no-publish"] {
		return nil
	}
	if err := built.Cell.PublishCapabilities(); err != nil {
		// Publishing needs a repository. Reporting the capabilities is still
		// useful without one — that is what `--no-publish` is for — so this is a
		// message, not a failure of the command's main purpose.
		return fmt.Errorf("could not publish to %s: %w", cell.CapabilitiesPrefix+caps.CellID+"/capabilities", err)
	}
	fmt.Println("published")
	return nil
}

func cmdOnce(args []string) error {
	f, err := parseFlags(args, nil, nil)
	if err != nil {
		return err
	}
	_, built, err := load(f)
	if err != nil {
		return err
	}
	ctx, cancel := signalContext()
	defer cancel()
	built.Cell.Log = logLine
	if err := built.Cell.Validate(ctx); err != nil {
		return err
	}
	rep, err := built.Cell.Once(ctx)
	if err != nil {
		return err
	}
	fmt.Println(rep.Summary())
	for _, a := range rep.Attempts {
		fmt.Printf("attempt %s #%d -> %s (env %s, cost %.4g)\n",
			shortID(a.Task), a.N, shortID(a.Change), shortID(a.Environment), a.Cost)
	}
	for _, v := range rep.Verified {
		fmt.Printf("verified %s (task %s)\n", shortID(v.Attempt), shortID(v.Task))
	}
	for _, p := range rep.Promotions {
		fmt.Println(p.Summary())
	}
	return nil
}

func cmdRun(args []string) error {
	f, err := parseFlags(args, []string{"interval"}, nil)
	if err != nil {
		return err
	}
	_, built, err := load(f)
	if err != nil {
		return err
	}
	interval := built.Interval
	if v := f.values["interval"]; v != "" {
		if interval, err = time.ParseDuration(v); err != nil {
			return err
		}
	}
	ctx, cancel := signalContext()
	defer cancel()
	built.Cell.Log = logLine
	logLine(fmt.Sprintf("cell %s starting, interval %s", built.Cell.Capabilities.CellID, interval))
	err = built.Cell.Run(ctx, interval)
	if errors.Is(err, context.Canceled) {
		logLine("interrupted; stopping after the current pass")
		return nil
	}
	return err
}

// cmdVerify runs verification only — the Micro cell's whole job, exposed as its
// own command so a verify/build cell can be run without the attempt path being
// present in the invocation at all (§10.3: prove the verification cell before
// the attempting cell).
func cmdVerify(args []string) error {
	f, err := parseFlags(args, nil, nil)
	if err != nil {
		return err
	}
	_, built, err := load(f)
	if err != nil {
		return err
	}
	ctx, cancel := signalContext()
	defer cancel()
	built.Cell.Log = logLine
	if err := built.Cell.Validate(ctx); err != nil {
		return err
	}
	attempts, err := built.Cell.PeerAttempts()
	if err != nil {
		return err
	}
	if len(attempts) == 0 {
		fmt.Println("no peer attempts to verify")
		return nil
	}
	verified := 0
	for _, pa := range attempts {
		if pa.Attempt.Change == "" {
			continue
		}
		ev, err := built.Cell.Reverify(ctx, pa.Attempt)
		if err != nil {
			fmt.Fprintf(os.Stderr, "verify %s: %v\n", shortID(pa.Attempt.Change), err)
			continue
		}
		status := "pass"
		if !ev.Passed() {
			status = "not-pass " + strings.Join(ev.Failures(), " ")
		}
		fmt.Printf("%s attempt %d by %s: %s\n", shortID(pa.Attempt.Change), pa.Attempt.N, pa.Attempt.CellID, status)
		verified++
	}
	fmt.Printf("verified %d attempt(s)\n", verified)
	return nil
}

func cmdPromote(args []string) error {
	f, err := parseFlags(args, []string{"mode", "enable", "disable"}, []string{"status"})
	if err != nil {
		return err
	}
	_, built, err := load(f)
	if err != nil {
		return err
	}

	if mode := f.values["mode"]; mode != "" {
		m, err := promote.ParseMode(mode)
		if err != nil {
			return err
		}
		if err := built.Switch.SetMode(m); err != nil {
			return err
		}
		// The switch is re-read on every promotion decision, so a running loop
		// picks this up on its next one — no restart and no signal (§6.5).
		fmt.Printf("promotion mode is now %s; this takes effect on the next promotion decision, in this process and in any running loop\n", m)
		if m == promote.ModeGated {
			fmt.Println("nothing will be promoted by this cell until a human decides")
		}
	}
	if path := f.values["enable"]; path != "" {
		if err := built.Switch.EnableAutonomous(path); err != nil {
			return err
		}
		fmt.Printf("autonomous promotion enabled for scope %s\n", path)
		// Enabling a path is not the same as being able to use it. Report the
		// agreement gate now rather than letting an operator discover it at the
		// first deferred promotion.
		rate, err := agreement.RateFor(built.Varvig, path)
		if err == nil {
			verdict := agreement.NewGate(0, 0).Allow(rate)
			fmt.Println(verdict.String())
		}
	}
	if path := f.values["disable"]; path != "" {
		if err := built.Switch.DisableAutonomous(path); err != nil {
			return err
		}
		fmt.Printf("autonomous promotion disabled for scope %s\n", path)
	}

	if f.bools["status"] || len(args) == 0 || onlyConfigFlag(args) {
		mode, err := built.Switch.Mode()
		if err != nil {
			return err
		}
		paths, err := built.Switch.AutonomousPaths()
		if err != nil {
			return err
		}
		fmt.Printf("mode     %s\n", mode)
		if len(paths) == 0 {
			fmt.Println("enabled  (none — autonomous mode is per-path and no path is enabled)")
		} else {
			fmt.Printf("enabled  %s\n", strings.Join(paths, " "))
		}
	}
	return nil
}

// onlyConfigFlag reports whether the args carried nothing but -c/--config, so a
// bare `promote` prints status instead of silently doing nothing.
func onlyConfigFlag(args []string) bool {
	for i := 0; i < len(args); i++ {
		if args[i] == "-c" || args[i] == "--config" {
			i++
			continue
		}
		return false
	}
	return true
}

func cmdAgreement(args []string) error {
	f, err := parseFlags(args, []string{"scope"}, nil)
	if err != nil {
		return err
	}
	_, built, err := load(f)
	if err != nil {
		return err
	}
	g := agreement.NewGate(0, 0)
	if scope := f.values["scope"]; scope != "" {
		rate, err := agreement.RateFor(built.Varvig, scope)
		if err != nil {
			return err
		}
		fmt.Println(rate.String())
		fmt.Println(g.Allow(rate).String())
		return nil
	}
	rates, err := agreement.Rates(built.Varvig)
	if err != nil {
		return err
	}
	report := agreement.Report(rates)
	if len(report) == 0 {
		fmt.Println("no promotion-agreement observations recorded yet")
		fmt.Println("run gated: each promotion records whether the top-scoring attempt was the promoted one (§6.4)")
		return nil
	}
	// Worst first: the scope an operator needs to look at is the first line.
	for _, r := range report {
		verdict := g.Allow(r)
		mark := "refused "
		if verdict.Allowed {
			mark = "eligible"
		}
		fmt.Printf("%s  %s\n", mark, r)
	}
	return nil
}

func cmdBudget(args []string) error {
	f, err := parseFlags(args, nil, nil)
	if err != nil {
		return err
	}
	cfg, built, err := load(f)
	if err != nil {
		return err
	}
	now := time.Now()
	snap := built.Ledger.Snapshot(now)
	b := built.Ledger.Budget()
	fmt.Printf("day               %s\n", snap.Day)
	fmt.Printf("inference spent   %.4g of %.4g\n", snap.Spent, snap.Cap)
	fmt.Printf("offline spent     %.4g of %.4g\n", snap.OfflineSpent, snap.OfflineCap)
	fmt.Printf("calls             %d\n", snap.Calls)
	fmt.Printf("verify concurrent %d\n", b.VerifyConcurrent)
	fmt.Printf("storage cap       %.4g GB\n", b.StorageGB)
	fmt.Printf("attempts default  %d\n", b.AttemptsDefault)
	// A verify/build cell has no inference budget by design, and reporting that
	// as "halted" would make the recommended default profile look broken. The
	// ledger's answer is the same either way; what differs is what it means.
	if !cfg.Capabilities().Has(cell.RoleAttempt) {
		fmt.Println("inference         n/a — this cell verifies and builds, and does not attempt (§3.1)")
		return nil
	}
	online := built.Ledger.CanSpend(now, false)
	offline := built.Ledger.CanSpend(now, true)
	fmt.Printf("online            %s\n", online)
	fmt.Printf("offline           %s\n", offline)
	if !online.OK {
		// A cell out of budget stops claiming and says so (§7). It does not
		// switch to a worse model.
		fmt.Println("this cell has stopped claiming; it will not switch to a smaller model to keep working")
	}
	return nil
}

func cmdGate(args []string) error {
	f, err := parseFlags(args, []string{"bind"}, []string{"show"})
	if err != nil {
		return err
	}
	cfg, built, err := load(f)
	if err != nil {
		return err
	}
	module := f.values["bind"]
	if module == "" {
		module = cfg.Promotion.GateModule
	}
	if module == "" {
		fmt.Printf("no gate module configured; bind one with --bind, or set promotion.gate_module\n")
		fmt.Printf("an unconfigured gate is not an approving gate: autonomous promotion refuses without one (§6.2)\n")
		return nil
	}
	id, err := built.Gate.Bind(module)
	if err != nil {
		return err
	}
	fmt.Printf("bound %s to event %s as %s\n", module, gate.Event, id)
	fmt.Println("the module is a repository object now: the promotion rule is versioned and auditable")
	return nil
}

func signalContext() (context.Context, context.CancelFunc) {
	ctx, cancel := context.WithCancel(context.Background())
	ch := make(chan os.Signal, 1)
	signal.Notify(ch, os.Interrupt, syscall.SIGTERM)
	go func() {
		<-ch
		cancel()
	}()
	return ctx, func() {
		signal.Stop(ch)
		cancel()
	}
}

func logLine(s string) {
	fmt.Fprintf(os.Stderr, "%s %s\n", time.Now().UTC().Format(time.RFC3339), s)
}

func shortID(s string) string {
	if len(s) <= 16 {
		return s
	}
	return s[:16] + "…"
}
