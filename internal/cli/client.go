package cli

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"time"

	"connectrpc.com/connect"
	"github.com/spf13/cobra"

	"github.com/richard-sen27/mycelium/gen/mycel/v1/mycelv1connect"
)

// hubClient bundles the Connect clients for one hub.
type hubClient struct {
	url    string
	tokens mycelv1connect.TokenServiceClient
	nodes  mycelv1connect.NodeServiceClient
	users  mycelv1connect.UserServiceClient
	chans  mycelv1connect.ChannelServiceClient
	agents mycelv1connect.AgentServiceClient
	// stream is the channel service on an HTTP client without an overall
	// timeout, for Watch.
	stream mycelv1connect.ChannelServiceClient
	jobs   mycelv1connect.JobServiceClient
	// jobStream is the job service without an overall timeout, for StreamLogs.
	jobStream mycelv1connect.JobServiceClient
	arts      mycelv1connect.ArtifactServiceClient
	grants    mycelv1connect.GrantServiceClient
	control   mycelv1connect.ControlServiceClient
	events    mycelv1connect.EventServiceClient
	// raw carries artifact bytes over plain HTTPS (PUT/GET /api/artifacts):
	// the pinned CA, no overall timeout; authz is its Authorization header.
	raw   *http.Client
	authz bearer
}

const loginHint = "run `mycel login <hub-url> --api-key <key>`, or `mycel hub init` on the hub machine"

// hub returns clients for the configured hub, failing with a helpful message
// when hub_url, api_key or ca_cert_pem are missing.
func (a *app) hub(cmd *cobra.Command) (*hubClient, error) {
	dirs, f, err := a.loadConfig(cmd)
	if err != nil {
		return nil, err
	}
	var missing []string
	if f.HubURL == "" {
		missing = append(missing, "hub_url")
	}
	if f.APIKey == "" {
		missing = append(missing, "api_key")
	}
	if f.CACertPEM == "" {
		missing = append(missing, "ca_cert_pem")
	}
	if len(missing) > 0 {
		return nil, fmt.Errorf("not connected to a hub (%s missing in %s): %s",
			strings.Join(missing, ", "), dirs.ConfigFile(), loginHint)
	}
	return newHubClient(f.HubURL, f.APIKey, []byte(f.CACertPEM))
}

func newHubClient(hubURL, apiKey string, caPEM []byte) (*hubClient, error) {
	base, err := normalizeHubURL(hubURL)
	if err != nil {
		return nil, err
	}
	hc, err := pinnedHTTPClient(caPEM)
	if err != nil {
		return nil, err
	}
	sc := *hc
	sc.Timeout = 0
	opts := connect.WithInterceptors(bearerInterceptor(apiKey))
	return &hubClient{
		url:    base,
		tokens: mycelv1connect.NewTokenServiceClient(hc, base, opts),
		nodes:  mycelv1connect.NewNodeServiceClient(hc, base, opts),
		users:  mycelv1connect.NewUserServiceClient(hc, base, opts),
		chans:  mycelv1connect.NewChannelServiceClient(hc, base, opts),
		agents: mycelv1connect.NewAgentServiceClient(hc, base, opts),
		stream: mycelv1connect.NewChannelServiceClient(&sc, base, opts),

		jobs:      mycelv1connect.NewJobServiceClient(hc, base, opts),
		jobStream: mycelv1connect.NewJobServiceClient(&sc, base, opts),
		arts:      mycelv1connect.NewArtifactServiceClient(hc, base, opts),
		grants:    mycelv1connect.NewGrantServiceClient(hc, base, opts),
		control:   mycelv1connect.NewControlServiceClient(hc, base, opts),
		events:    mycelv1connect.NewEventServiceClient(hc, base, opts),
		raw:       &sc,
		authz:     bearer{header: "Bearer " + apiKey},
	}, nil
}

// normalizeHubURL checks that u is an https URL and strips a trailing slash.
func normalizeHubURL(u string) (string, error) {
	p, err := url.Parse(u)
	if err != nil || p.Host == "" {
		return "", fmt.Errorf("invalid hub URL %q: want https://host[:port]", u)
	}
	if p.Scheme != "https" {
		return "", fmt.Errorf("invalid hub URL %q: the hub API is only reachable over https", u)
	}
	return strings.TrimRight(u, "/"), nil
}

// pinnedHTTPClient trusts only the given CA, never the system roots.
func pinnedHTTPClient(caPEM []byte) (*http.Client, error) {
	pool := x509.NewCertPool()
	if !pool.AppendCertsFromPEM(caPEM) {
		return nil, errors.New("config has no valid hub CA certificate (ca_cert_pem): " + loginHint)
	}
	tr := &http.Transport{
		Proxy:               http.ProxyFromEnvironment,
		TLSClientConfig:     &tls.Config{RootCAs: pool, MinVersion: tls.VersionTLS12},
		ForceAttemptHTTP2:   true,
		TLSHandshakeTimeout: 10 * time.Second,
	}
	return &http.Client{Transport: tr, Timeout: 60 * time.Second}, nil
}

// bearerInterceptor sets the API key on every request, unary and streaming.
// The key is never logged.
func bearerInterceptor(apiKey string) connect.Interceptor {
	return bearer{header: "Bearer " + apiKey}
}

// bearer holds the Authorization header; String and GoString redact it so a
// formatted value never prints the key.
type bearer struct{ header string }

func (bearer) String() string   { return "Bearer [redacted]" }
func (bearer) GoString() string { return "bearer{[redacted]}" }

func (b bearer) WrapUnary(next connect.UnaryFunc) connect.UnaryFunc {
	return func(ctx context.Context, req connect.AnyRequest) (connect.AnyResponse, error) {
		req.Header().Set("Authorization", b.header)
		return next(ctx, req)
	}
}

func (b bearer) WrapStreamingClient(next connect.StreamingClientFunc) connect.StreamingClientFunc {
	return func(ctx context.Context, spec connect.Spec) connect.StreamingClientConn {
		conn := next(ctx, spec)
		conn.RequestHeader().Set("Authorization", b.header)
		return conn
	}
}

func (b bearer) WrapStreamingHandler(next connect.StreamingHandlerFunc) connect.StreamingHandlerFunc {
	return next
}

// rpcError turns a Connect error into a message a person can act on.
func rpcError(hubURL string, err error) error {
	var ce *connect.Error
	if !errors.As(err, &ce) {
		return err
	}
	msg := ce.Message()
	switch ce.Code() {
	case connect.CodeUnauthenticated:
		return fmt.Errorf("authentication failed: %s (check your API key, or %s)", msg, loginHint)
	case connect.CodePermissionDenied:
		return fmt.Errorf("permission denied: %s", msg)
	case connect.CodeNotFound:
		return fmt.Errorf("not found: %s", msg)
	case connect.CodeAlreadyExists:
		return fmt.Errorf("already exists: %s", msg)
	case connect.CodeInvalidArgument, connect.CodeFailedPrecondition:
		return fmt.Errorf("rejected by hub: %s", msg)
	case connect.CodeUnavailable:
		return fmt.Errorf("hub unreachable at %s: %s", hubURL, msg)
	case connect.CodeDeadlineExceeded:
		return fmt.Errorf("hub at %s did not answer in time", hubURL)
	default:
		return fmt.Errorf("hub error (%s): %s", ce.Code(), msg)
	}
}
