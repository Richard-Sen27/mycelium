package cli

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"

	"github.com/spf13/cobra"

	"github.com/richard-sen27/mycelium/internal/config"
	"github.com/richard-sen27/mycelium/internal/daemon"
	"github.com/richard-sen27/mycelium/internal/hub"
)

const (
	defaultLocalAddr = "127.0.0.1:7777"
	// hubMachineLocalAddr is the node's local UI address when the hub role runs
	// in the same process: the hub API already owns :7777.
	hubMachineLocalAddr = "127.0.0.1:7779"
)

type daemonReadyJSON struct {
	Hub  *hubReadyJSON  `json:"hub,omitempty"`
	Node *nodeReadyJSON `json:"node,omitempty"`
}

type hubReadyJSON struct {
	APIAddr string `json:"api_addr"`
	BusAddr string `json:"bus_addr"`
}

type nodeReadyJSON struct {
	LocalAddr string `json:"local_addr"`
}

type roleResult struct {
	role string
	err  error
}

func (a *app) daemonCmd() *cobra.Command {
	var apiListen, busListen, localAddr string
	var allowFakeProfile bool
	cmd := &cobra.Command{
		Use:   "daemon",
		Short: "Run every role initialised on this machine (hub and/or node)",
		Args:  noArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			dirs, err := config.Resolve()
			if err != nil {
				return err
			}
			runHub := fileExists(filepath.Join(dirs.HubDir(), "hub.yaml"))
			runNode := fileExists(filepath.Join(dirs.NodeDir(), "node.yaml"))
			if !runHub && !runNode {
				return errors.New("nothing to run: `mycel hub init` or `mycel join` first")
			}
			if runHub && !cmd.Flags().Changed("local-addr") {
				localAddr = hubMachineLocalAddr
			}
			if !runHub && (cmd.Flags().Changed("api-listen") || cmd.Flags().Changed("bus-listen")) {
				a.log.Warn("--api-listen and --bus-listen only apply to the hub role, which is not initialised here")
			}
			if allowFakeProfile {
				if runHub {
					a.log.Warn("--allow-fake-profile: the CI-only fake sandbox profile (no isolation) is loaded")
				} else {
					a.log.Warn("--allow-fake-profile only applies to the hub role, which is not initialised here")
				}
			}

			sigCtx, stop := signal.NotifyContext(cmd.Context(), os.Interrupt, syscall.SIGTERM)
			defer stop()
			ctx, cancel := context.WithCancel(sigCtx)
			defer cancel()
			go func() {
				// Once shutdown starts, a second signal gets the default
				// handling and kills the process if shutdown hangs.
				<-ctx.Done()
				stop()
			}()

			done := make(chan roleResult, 2)
			running := 0
			hubReady := make(chan hub.Ready, 1)
			nodeReady := make(chan daemon.Ready, 1)
			start := func(role string, run func() error) {
				running++
				go func() {
					err := run()
					if err == nil && ctx.Err() == nil {
						err = errors.New("exited unexpectedly")
					}
					done <- roleResult{role, err}
				}()
			}
			if runHub {
				opts := hub.Options{HubDir: dirs.HubDir(), APIListen: apiListen, BusListen: busListen, Logger: a.log.With("role", "hub"),
					AllowFakeProfile: allowFakeProfile}
				start("hub", func() error { return a.hubRun(ctx, opts, hubReady) })
			}
			if runNode {
				opts := daemon.Options{NodeDir: dirs.NodeDir(), LocalAddr: localAddr, Logger: a.log.With("role", "node")}
				start("node", func() error { return a.daemonRun(ctx, opts, nodeReady) })
			}

			var results []roleResult
			finish := func(r roleResult) {
				running--
				results = append(results, r)
				if r.err != nil && !errors.Is(r.err, context.Canceled) {
					a.log.Error("role stopped", "role", r.role, "err", r.err)
				} else {
					a.log.Info("role stopped", "role", r.role)
				}
				cancel() // one role ending stops the others
			}

			out := daemonReadyJSON{}
			waitHub, waitNode := runHub, runNode
			onHubReady := func(r hub.Ready) {
				waitHub = false
				out.Hub = &hubReadyJSON{APIAddr: r.APIAddr, BusAddr: r.BusAddr}
				a.log.Info("hub ready", "api_addr", r.APIAddr, "bus_addr", r.BusAddr, "hub_url", r.HubURL)
			}
			onNodeReady := func(r daemon.Ready) {
				waitNode = false
				out.Node = &nodeReadyJSON{LocalAddr: r.LocalAddr}
				a.log.Info("node ready", "local_addr", r.LocalAddr)
			}
		waitReady:
			for waitHub || waitNode {
				select {
				case r := <-hubReady:
					onHubReady(r)
				case r := <-nodeReady:
					onNodeReady(r)
				case r := <-done:
					finish(r)
					break waitReady
				case <-ctx.Done():
					break waitReady
				}
			}
			printReady := func() {
				if !waitHub && !waitNode && a.jsonOut {
					if err := a.printJSONValue(out); err != nil {
						a.log.Error("write ready line", "err", err)
					}
				}
			}
			if !waitHub && !waitNode {
				printReady()
				for running > 0 {
					finish(<-done)
				}
				return daemonExit(results)
			}
			// Stopped early: wait for every role to return, then collect any
			// ready reports that were sent before they stopped (select picks
			// among ready cases at random).
			for running > 0 {
				finish(<-done)
			}
			select {
			case r := <-hubReady:
				onHubReady(r)
			default:
			}
			select {
			case r := <-nodeReady:
				onNodeReady(r)
			default:
			}
			printReady()
			return daemonExit(results)
		},
	}
	f := cmd.Flags()
	f.StringVar(&apiListen, "api-listen", "", "hub API listen address (overrides hub.yaml)")
	f.StringVar(&busListen, "bus-listen", "", "hub bus listen address (overrides hub.yaml)")
	f.StringVar(&localAddr, "local-addr", defaultLocalAddr, "node local UI and status address (default "+hubMachineLocalAddr+" when the hub role also runs)")
	f.BoolVar(&allowFakeProfile, "allow-fake-profile", false, "load the CI-only \"fake\" sandbox profile (no isolation; tests only)")
	_ = f.MarkHidden("allow-fake-profile")
	return cmd
}

// daemonExit maps the roles' results to the exit code: 0 on a clean stop,
// 3 if the node was revoked, 1 otherwise.
func daemonExit(results []roleResult) error {
	var firstErr error
	for _, r := range results {
		if errors.Is(r.err, daemon.ErrRevoked) {
			return exitCodeError{code: exitRevoked, err: fmt.Errorf("%s: %w", r.role, r.err)}
		}
		if r.err != nil && !errors.Is(r.err, context.Canceled) && firstErr == nil {
			firstErr = fmt.Errorf("%s: %w", r.role, r.err)
		}
	}
	return firstErr
}

func fileExists(p string) bool {
	_, err := os.Stat(p)
	return err == nil
}
