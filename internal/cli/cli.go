// Package cli implements the mycel command-line interface (cobra).
//
// Contract (owned by the orchestrator): the command tree below is the phase-1
// surface. Flags in brackets are optional. Every command supports --json for
// machine-readable output; the e2e tests depend on it.
//
//	mycel version
//	mycel hub init  --advertise <url> [--trust-domain <td>] [--api-listen :7777] [--bus-listen :7778] [--admin <name>] [--node-name <name>]
//	                 prints the admin API key once and writes config.yaml (hub_url, api_key, ca pem)
//	mycel join <token> [--hub-url <url>]
//	mycel daemon    [--api-listen] [--bus-listen] [--local-addr 127.0.0.1:7777]
//	                 runs every role present in the data dir (docs/decisions.md D006)
//	mycel token create --name <node> [--caps docker,ollama,gpu] [--ttl 15m]
//	mycel token ls
//	mycel token revoke <id>
//	mycel nodes ls
//	mycel nodes get <name>
//	mycel nodes rm <name>
//	mycel nodes caps <name> --set docker,ollama
//	mycel user add <name> --role admin|member      (prints the new user's API key once)
//	mycel user ls
//	mycel user rm <id>
//	mycel whoami
//	mycel login <hub-url> --api-key <key> [--ca-fingerprint <hex>]
//	                 fetches <hub-url>/ca.pem, checks the fingerprint (asks if not given, unless --json), writes config.yaml
//	mycel ui         prints (and tries to open) the local UI URL
//
// Global flags: --json, --hub-url, --api-key (override config.yaml), -v.
// Environment: MYCEL_HOME (see internal/config), MYCEL_API_KEY, MYCEL_HUB_URL.
package cli

import "fmt"

// Main runs the CLI with the given arguments and returns the process exit code.
func Main(args []string) int {
	fmt.Println("mycel: not implemented")
	return 1
}
