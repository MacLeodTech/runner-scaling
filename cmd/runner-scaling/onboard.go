package main

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	"github.com/MacLeodTech/runner-scaling/internal/onboard"
)

func printChecks(cs []onboard.Check) (fails, warns int) {
	for _, c := range cs {
		mark := map[onboard.Level]string{onboard.Pass: "  ok  ", onboard.Warn: " WARN ", onboard.Fail: " FAIL ", onboard.Skip: " skip "}[c.Level]
		line := mark + c.Name
		if c.Detail != "" {
			line += ": " + c.Detail
		}
		fmt.Println(line)
		if c.Fix != "" && c.Level != onboard.Pass {
			fmt.Println("        fix: " + c.Fix)
		}
		switch c.Level {
		case onboard.Fail:
			fails++
		case onboard.Warn:
			warns++
		}
	}
	return
}

func runDoctor(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("doctor", flag.ExitOnError)
	dir := fs.String("dir", "", "also check this stack directory (compose.yaml, .env, guard*.yaml, secrets/)")
	offline := fs.Bool("offline", false, "skip the checks that need the internet")
	sysbox := fs.Bool("sysbox", false, "require the Sysbox runtime (for a pool with Docker in jobs)")
	gh := fs.Bool("github", false, "also ask GitHub about the App (needs --dir; reads the key, so run as its owner or with sudo)")
	asJSON := fs.Bool("json", false, "print the findings as JSON")
	sock := fs.String("docker", env("DOCKER_SOCKET", "/var/run/docker.sock"), "Docker socket")
	fs.Parse(args)
	cs := onboard.Doctor{Dir: *dir, Offline: *offline, WantSysbox: *sysbox, GitHub: *gh, DockerHost: *sock}.Run(ctx)
	if *asJSON {
		return json.NewEncoder(os.Stdout).Encode(cs)
	}
	fails, warns := printChecks(cs)
	fmt.Printf("\n%d problem(s) to fix, %d warning(s).\n", fails, warns)
	if fails > 0 {
		os.Exit(1)
	}
	return nil
}

func runInit(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("init", flag.ExitOnError)
	org := fs.String("org", "", "your GitHub organization (required)")
	dir := fs.String("dir", ".", "where the stack lives (compose.yaml, .env, guard-lite.yaml, secrets/)")
	create := fs.Bool("create-app", false, "create the GitHub App through your browser (GitHub's manifest flow)")
	appName := fs.String("app-name", "runner-scaler", "name for the new App")
	listen := fs.String("listen", "127.0.0.1:0", "address for the local page (use a fixed port plus `ssh -L` on a headless host)")
	clientID := fs.String("client-id", "", "existing App: Client ID")
	instID := fs.Int64("installation-id", 0, "existing App: installation id")
	keyFile := fs.String("key-file", "", "existing App: private key (.pem); copied into secrets/")
	force := fs.Bool("force", false, "overwrite files that exist")
	dry := fs.Bool("dry-run", false, "show what would be written; write nothing")
	sock := fs.String("docker", env("DOCKER_SOCKET", "/var/run/docker.sock"), "Docker socket")
	fs.Parse(args)
	if *org == "" {
		return errors.New("--org is required, e.g. runner-scaling init --org my-org --create-app")
	}
	d := onboard.Doctor{DockerHost: *sock}
	host, err := d.Detect(ctx)
	if err != nil {
		return fmt.Errorf("%w\n(run `runner-scaling doctor` to see what is missing)", err)
	}
	var key []byte
	switch {
	case *create:
		if *clientID != "" || *keyFile != "" {
			return errors.New("--create-app and --client-id/--key-file are alternatives")
		}
		if *dry {
			return errors.New("--dry-run cannot create an App")
		}
		fmt.Println("Creating the GitHub App for", *org, "...")
		f := &onboard.AppFlow{Org: *org, Name: *appName, Listen: *listen}
		f.Announce = func(u string) {
			fmt.Printf("\nOpen this address in a browser that is signed in to GitHub as an owner of %s:\n\n    %s\n\n", *org, u)
			fmt.Println("(On a remote host, forward the port first: ssh -L <port>:127.0.0.1:<port> <this host>.)")
			fmt.Println("Press the button, then \"Create GitHub App\", then install the App on the organization. Waiting...")
		}
		res, err := f.Run(ctx)
		if err != nil {
			return err
		}
		fmt.Printf("App %q created and installed on %s (installation %d).\n", res.Slug, res.Account, res.InstallationID)
		*clientID, *instID, key = res.ClientID, res.InstallationID, res.PEM
	case *clientID != "" && *instID != 0 && *keyFile != "":
		if key, err = os.ReadFile(*keyFile); err != nil {
			return err
		}
	default:
		return errors.New("give either --create-app, or --client-id, --installation-id and --key-file of an App you made (QUICKSTART step 3)")
	}
	plan, err := onboard.NewPlan(onboard.PlanInput{Org: *org, ClientID: *clientID, InstallationID: *instID, Key: key,
		DockerGID: host.DockerGID, MemTotalGB: host.MemTotalGB, Taken: host.Taken, Version: version, Root: os.Geteuid() == 0})
	if err != nil {
		return err
	}
	for _, n := range plan.Notes {
		fmt.Println(" -", n)
	}
	if *dry {
		for name, f := range plan.Files {
			fmt.Printf("would write %s (%04o, %d bytes)\n", filepath.Join(*dir, name), f.Mode, len(f.Data))
		}
		return nil
	}
	notes, err := plan.Write(*dir, *force)
	if err != nil {
		return err
	}
	fmt.Println("Wrote the stack to", *dir)
	fmt.Println("\nChecking it:")
	fails, _ := printChecks(onboard.Doctor{Dir: *dir, DockerHost: *sock}.Run(ctx))
	for _, n := range notes {
		fmt.Println("\n" + n)
	}
	fmt.Printf("\nNext:\n  cd %s && docker compose up -d && docker compose logs -f\n", *dir)
	fmt.Println("Then run one job on the pool and pin the tenant: runner-scaling pin-tenant --dir", *dir, " (QUICKSTART step 6).")
	if fails > 0 {
		fmt.Println("\nFix the FAIL lines above first (the key's owner is the usual one on a non-root run).")
	}
	return nil
}

func runPinTenant(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("pin-tenant", flag.ExitOnError)
	dir := fs.String("dir", ".", "the stack directory")
	cfgName := fs.String("config", "guard-lite.yaml", "the guard config to edit")
	guardSvc := fs.String("guard", "guard-lite", "the guard's compose service")
	policySvc := fs.String("policy", "docker-policy", "the docker-policy's compose service")
	yes := fs.Bool("yes", false, "do not ask for confirmation")
	fs.Parse(args)
	compose := func(a ...string) *exec.Cmd {
		c := exec.CommandContext(ctx, "docker", append([]string{"compose", "-f", filepath.Join(*dir, "compose.yaml"), "--project-directory", *dir}, a...)...)
		return c
	}
	logs, err := compose("logs", "--no-color", "--tail", "500", *guardSvc, *policySvc).CombinedOutput()
	if err != nil {
		return fmt.Errorf("reading logs: %v: %s", err, strings.TrimSpace(string(logs)))
	}
	tenant, err := onboard.FindTenant(string(logs))
	if err != nil {
		return err
	}
	fmt.Println("The guard refused a registration pointing at:\n\n    " + tenant + "\n")
	fmt.Println("Pin it only if this host and this App are yours and nothing untrusted could have reached the scaler yet:")
	fmt.Println("from now on the guard accepts runners for this tenant and refuses every other organization.")
	if !*yes {
		fmt.Print("Pin it? [y/N] ")
		ans, _ := bufio.NewReader(os.Stdin).ReadString('\n')
		if a := strings.ToLower(strings.TrimSpace(ans)); a != "y" && a != "yes" {
			return errors.New("not pinned")
		}
	}
	path := filepath.Join(*dir, *cfgName)
	b, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	out, err := onboard.SetTenant(string(b), tenant)
	if err != nil {
		return err
	}
	if err := os.WriteFile(path, []byte(out), 0o644); err != nil {
		return err
	}
	fmt.Println("Updated", path, "; restarting", *policySvc, "and", *guardSvc, "(both read the file once).")
	if o, err := compose("restart", *policySvc, *guardSvc).CombinedOutput(); err != nil {
		return fmt.Errorf("restart: %v: %s", err, o)
	}
	fmt.Println("Done. The scaler retries by itself within about 15 seconds.")
	return nil
}
