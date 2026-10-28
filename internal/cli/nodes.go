package cli

import (
	"fmt"
	"strconv"
	"strings"

	"connectrpc.com/connect"
	"github.com/spf13/cobra"

	mycelv1 "github.com/richard-sen27/mycelium/gen/mycel/v1"
)

func (a *app) nodesCmd() *cobra.Command {
	cmd := groupCmd("nodes", "List and manage enrolled nodes", "node")
	cmd.AddCommand(a.nodesLsCmd(), a.nodesGetCmd(), a.nodesRmCmd(), a.nodesCapsCmd())
	return cmd
}

func (a *app) nodesLsCmd() *cobra.Command {
	return &cobra.Command{
		Use:     "ls",
		Aliases: []string{"list"},
		Short:   "List nodes with their inventory",
		Args:    noArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			hc, err := a.hub(cmd)
			if err != nil {
				return err
			}
			resp, err := hc.nodes.ListNodes(cmd.Context(), connect.NewRequest(&mycelv1.ListNodesRequest{}))
			if err != nil {
				return rpcError(hc.url, err)
			}
			if a.jsonOut {
				return a.printProto(resp.Msg)
			}
			now := a.now()
			tw := newTable(a.stdout)
			row(tw, "NAME", "OWNER", "ONLINE", "OS/ARCH", "CPU", "RAM", "GPUS", "CAPS", "LAST SEEN")
			for _, n := range resp.Msg.GetNodes() {
				inv := n.GetInventory()
				osArch, cpu := "-", "-"
				if inv.GetOs() != "" || inv.GetArch() != "" {
					osArch = dash(inv.GetOs()) + "/" + dash(inv.GetArch())
				}
				if inv.GetCpuCount() > 0 {
					cpu = strconv.Itoa(int(inv.GetCpuCount()))
				}
				row(tw, n.GetName(), dash(n.GetOwnerId()), yesNo(n.GetOnline()), osArch, cpu,
					bytesString(inv.GetMemBytes()), strconv.Itoa(len(inv.GetGpus())),
					capsString(n.GetCapsEnabled()), ago(n.GetLastSeenAt(), now))
			}
			return tw.Flush()
		},
	}
}

func (a *app) nodesGetCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "get <name>",
		Short: "Show one node in detail",
		Args:  exactArgs(1, "name"),
		RunE: func(cmd *cobra.Command, args []string) error {
			hc, err := a.hub(cmd)
			if err != nil {
				return err
			}
			resp, err := hc.nodes.GetNode(cmd.Context(), connect.NewRequest(&mycelv1.GetNodeRequest{Name: args[0]}))
			if err != nil {
				return rpcError(hc.url, err)
			}
			if a.jsonOut {
				return a.printProto(resp.Msg)
			}
			a.printNode(resp.Msg.GetNode())
			return nil
		},
	}
}

func (a *app) printNode(n *mycelv1.Node) {
	inv := n.GetInventory()
	tw := newTable(a.stdout)
	kv := func(k, v string) { row(tw, k+":", v) }
	kv("Name", n.GetName())
	kv("ID", dash(n.GetId()))
	kv("SPIFFE ID", dash(n.GetSpiffeId()))
	kv("Owner", dash(n.GetOwnerId()))
	kv("Online", yesNo(n.GetOnline()))
	kv("Last seen", ago(n.GetLastSeenAt(), a.now()))
	kv("Caps enabled", capsString(n.GetCapsEnabled()))
	kv("Caps ceiling", capsString(n.GetCapsCeiling()))
	kv("Cert serial", dash(n.GetCertSerial()))
	kv("Cert expires", dash(n.GetCertNotAfter()))
	kv("Created", dash(n.GetCreatedAt()))
	if inv != nil {
		kv("Hostname", dash(inv.GetHostname()))
		kv("OS", strings.TrimSpace(dash(inv.GetOs())+"/"+dash(inv.GetArch())+" "+inv.GetOsVersion()))
		kv("CPU", strings.TrimSpace(fmt.Sprintf("%d %s", inv.GetCpuCount(), inv.GetCpuModel())))
		kv("RAM", bytesString(inv.GetMemBytes()))
		for _, g := range inv.GetGpus() {
			kv(fmt.Sprintf("GPU %d", g.GetIndex()), strings.TrimSpace(fmt.Sprintf("%s %s %s", g.GetVendor(), g.GetModel(), bytesString(g.GetVramBytes()))))
		}
		kv("Docker", toolString(inv.GetDocker()))
		kv("Ollama", toolString(inv.GetOllama()))
		kv("Sandboxes", dash(strings.Join(inv.GetSandboxBackends(), ",")))
		kv("Mycel", dash(inv.GetMycelVersion()))
	}
	_ = tw.Flush()
}

func toolString(t *mycelv1.ToolPresence) string {
	if !t.GetPresent() {
		return "no"
	}
	return strings.TrimSpace("yes " + t.GetVersion())
}

func (a *app) nodesRmCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "rm <name>",
		Short: "Remove a node: revoke its certificate and disconnect it",
		Args:  exactArgs(1, "name"),
		RunE: func(cmd *cobra.Command, args []string) error {
			hc, err := a.hub(cmd)
			if err != nil {
				return err
			}
			resp, err := hc.nodes.RemoveNode(cmd.Context(), connect.NewRequest(&mycelv1.RemoveNodeRequest{Name: args[0]}))
			if err != nil {
				return rpcError(hc.url, err)
			}
			if a.jsonOut {
				return a.printProto(resp.Msg)
			}
			_, err = fmt.Fprintf(a.stdout, "Removed node %s; its certificate is revoked.\n", args[0])
			return err
		},
	}
}

func (a *app) nodesCapsCmd() *cobra.Command {
	var set []string
	cmd := &cobra.Command{
		Use:   "caps <name> --set docker,ollama",
		Short: "Set the capabilities a node offers (within its ceiling)",
		Args:  exactArgs(1, "name"),
		RunE: func(cmd *cobra.Command, args []string) error {
			if !cmd.Flags().Changed("set") {
				return usagef("--set is required (use --set '' to switch every capability off)")
			}
			hc, err := a.hub(cmd)
			if err != nil {
				return err
			}
			req := &mycelv1.SetNodeCapsRequest{Name: args[0], CapsEnabled: &mycelv1.Capabilities{Names: splitList(set)}}
			resp, err := hc.nodes.SetNodeCaps(cmd.Context(), connect.NewRequest(req))
			if err != nil {
				return rpcError(hc.url, err)
			}
			if a.jsonOut {
				return a.printProto(resp.Msg)
			}
			n := resp.Msg.GetNode()
			_, err = fmt.Fprintf(a.stdout, "Node %s now offers %s (ceiling %s).\n", args[0], capsString(n.GetCapsEnabled()), capsString(n.GetCapsCeiling()))
			return err
		},
	}
	cmd.Flags().StringSliceVar(&set, "set", nil, "capabilities to enable, comma-separated")
	return cmd
}
