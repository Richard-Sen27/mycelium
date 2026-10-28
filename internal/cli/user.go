package cli

import (
	"fmt"

	"connectrpc.com/connect"
	"github.com/spf13/cobra"

	mycelv1 "github.com/richard-sen27/mycelium/gen/mycel/v1"
)

func (a *app) userCmd() *cobra.Command {
	cmd := groupCmd("user", "Manage hub users", "users")
	cmd.AddCommand(a.userAddCmd(), a.userLsCmd(), a.userRmCmd())
	return cmd
}

func (a *app) userAddCmd() *cobra.Command {
	var role string
	cmd := &cobra.Command{
		Use:   "add <name> --role admin|member",
		Short: "Create a user and print their API key once",
		Args:  exactArgs(1, "name"),
		RunE: func(cmd *cobra.Command, args []string) error {
			r, err := parseRole(role)
			if err != nil {
				return err
			}
			hc, err := a.hub(cmd)
			if err != nil {
				return err
			}
			resp, err := hc.users.CreateUser(cmd.Context(), connect.NewRequest(&mycelv1.CreateUserRequest{Name: args[0], Role: r}))
			if err != nil {
				return rpcError(hc.url, err)
			}
			if a.jsonOut {
				return a.printProto(resp.Msg)
			}
			u := resp.Msg.GetUser()
			w := a.stdout
			_, _ = fmt.Fprintf(w, "Created %s %s (%s).\n", roleString(u.GetRole()), u.GetName(), u.GetId())
			_, _ = fmt.Fprintf(w, "API key (shown once; give it to %s for `mycel login`):\n\n", u.GetName())
			_, err = fmt.Fprintf(w, "  %s\n", resp.Msg.GetApiKey())
			return err
		},
	}
	cmd.Flags().StringVar(&role, "role", "member", "admin or member")
	return cmd
}

func (a *app) userLsCmd() *cobra.Command {
	return &cobra.Command{
		Use:     "ls",
		Aliases: []string{"list"},
		Short:   "List users",
		Args:    noArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			hc, err := a.hub(cmd)
			if err != nil {
				return err
			}
			resp, err := hc.users.ListUsers(cmd.Context(), connect.NewRequest(&mycelv1.ListUsersRequest{}))
			if err != nil {
				return rpcError(hc.url, err)
			}
			if a.jsonOut {
				return a.printProto(resp.Msg)
			}
			tw := newTable(a.stdout)
			row(tw, "ID", "NAME", "ROLE", "CREATED")
			for _, u := range resp.Msg.GetUsers() {
				row(tw, u.GetId(), u.GetName(), roleString(u.GetRole()), dash(u.GetCreatedAt()))
			}
			return tw.Flush()
		},
	}
}

func (a *app) userRmCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "rm <id>",
		Short: "Remove a user",
		Args:  exactArgs(1, "id"),
		RunE: func(cmd *cobra.Command, args []string) error {
			hc, err := a.hub(cmd)
			if err != nil {
				return err
			}
			resp, err := hc.users.RemoveUser(cmd.Context(), connect.NewRequest(&mycelv1.RemoveUserRequest{Id: args[0]}))
			if err != nil {
				return rpcError(hc.url, err)
			}
			if a.jsonOut {
				return a.printProto(resp.Msg)
			}
			_, err = fmt.Fprintf(a.stdout, "Removed user %s.\n", args[0])
			return err
		},
	}
}

func (a *app) whoamiCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "whoami",
		Short: "Show the user the API key belongs to",
		Args:  noArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			hc, err := a.hub(cmd)
			if err != nil {
				return err
			}
			resp, err := hc.users.Whoami(cmd.Context(), connect.NewRequest(&mycelv1.WhoamiRequest{}))
			if err != nil {
				return rpcError(hc.url, err)
			}
			if a.jsonOut {
				return a.printProto(resp.Msg)
			}
			a.printWhoami(resp.Msg)
			return nil
		},
	}
}

func (a *app) printWhoami(m *mycelv1.WhoamiResponse) {
	u := m.GetUser()
	tw := newTable(a.stdout)
	row(tw, "User:", fmt.Sprintf("%s (%s)", u.GetName(), u.GetId()))
	row(tw, "Role:", roleString(u.GetRole()))
	row(tw, "Hub:", dash(m.GetHubUrl()))
	row(tw, "Trust domain:", dash(m.GetTrustDomain()))
	row(tw, "CA fingerprint:", dash(m.GetCaFingerprint()))
	_ = tw.Flush()
}
