package cli

// Phase 4: artifacts (hub API). Metadata over Connect (ArtifactService),
// bytes over plain HTTPS on the same server (PUT /api/artifacts,
// GET /api/artifacts/{id}) with the API key and the pinned hub CA.
//
//	mycel artifacts ls [--job id] [--channel name]
//	mycel artifacts get <id> [-o path]
//	mycel artifacts put <path> [--name n] [--channel name]
//	mycel artifacts rm <id>

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"net/http"
	"net/url"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"

	"connectrpc.com/connect"
	"github.com/spf13/cobra"
	"google.golang.org/protobuf/encoding/protojson"

	mycelv1 "github.com/richard-sen27/mycelium/gen/mycel/v1"
	"github.com/richard-sen27/mycelium/internal/daemon"
)

// maxArtifactUpload is the hub's limit for PUT /api/artifacts (1 GiB).
const maxArtifactUpload = 1 << 30

func (a *app) artifactsCmd() *cobra.Command {
	cmd := groupCmd("artifacts", "List, upload, download and delete artifacts", "artifact", "art")
	cmd.AddCommand(a.artifactsLsCmd(), a.artifactsGetCmd(), a.artifactsPutCmd(), a.artifactsRmCmd())
	return cmd
}

func (a *app) artifactsLsCmd() *cobra.Command {
	var job, channel string
	cmd := &cobra.Command{
		Use:     "ls [--job id] [--channel name]",
		Aliases: []string{"list"},
		Short:   "List the artifacts you may see",
		Args:    noArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			req := &mycelv1.ListArtifactsRequest{JobId: job}
			if channel != "" {
				req.Channel = normChannel(channel)
			}
			hc, err := a.hub(cmd)
			if err != nil {
				return err
			}
			resp, err := hc.arts.ListArtifacts(cmd.Context(), connect.NewRequest(req))
			if err != nil {
				return rpcError(hc.url, err)
			}
			if a.jsonOut {
				return a.printProto(resp.Msg)
			}
			tw := newTable(a.stdout)
			row(tw, "ID", "NAME", "SIZE", "SHA256", "JOB", "CHANNEL", "CREATED BY", "CREATED")
			for _, ar := range resp.Msg.GetArtifacts() {
				sum := ar.GetSha256()
				if len(sum) > 12 {
					sum = sum[:12]
				}
				row(tw, ar.GetId(), dash(ar.GetName()), sizeCell(ar.GetSize()), dash(sum), dash(ar.GetJobId()),
					dash(ar.GetChannel()), dash(ar.GetCreatedBy()), ago(ar.GetCreatedAt(), a.now()))
			}
			return tw.Flush()
		},
	}
	cmd.Flags().StringVar(&job, "job", "", "only artifacts produced by this job")
	cmd.Flags().StringVar(&channel, "channel", "", "only artifacts attached to this channel")
	return cmd
}

func sizeCell(n int64) string {
	if n <= 0 {
		return "0 B"
	}
	return bytesString(uint64(n))
}

// artifactFileName turns an artifact's name (untrusted, from the hub) into a
// file name in the current directory that is safe on every OS, or "" when it
// cannot be one: the base name only, without controls or the characters
// Windows forbids, without trailing dots or spaces, never a Windows device
// name, and never hidden (a leading "." becomes "_").
func artifactFileName(name string) string {
	name = strings.ReplaceAll(name, `\`, "/")
	if i := strings.LastIndexByte(name, '/'); i >= 0 {
		name = name[i+1:]
	}
	name = strings.Map(func(r rune) rune {
		if isControl(r) || strings.ContainsRune(`:<>"|?*`, r) {
			return -1
		}
		return r
	}, name)
	name = strings.TrimRight(strings.TrimSpace(name), ". ")
	if name == "" || windowsDeviceName(name) {
		return ""
	}
	if strings.HasPrefix(name, ".") {
		name = "_" + name[1:]
	}
	return name
}

// windowsDeviceName reports names Windows maps to devices, with or without
// an extension ("CON", "nul.txt", "COM1", "LPT9.log", also with ¹²³).
func windowsDeviceName(name string) bool {
	base, _, _ := strings.Cut(name, ".")
	base = strings.ToUpper(strings.TrimRight(base, " "))
	switch base {
	case "CON", "PRN", "AUX", "NUL", "CONIN$", "CONOUT$":
		return true
	}
	if len(base) >= 4 && (strings.HasPrefix(base, "COM") || strings.HasPrefix(base, "LPT")) {
		switch base[3:] {
		case "0", "1", "2", "3", "4", "5", "6", "7", "8", "9", "¹", "²", "³":
			return true
		}
	}
	return false
}

// artifactHTTPError turns a non-2xx answer of the artifact endpoints into a
// readable error.
func artifactHTTPError(hubURL string, res *http.Response) error {
	if res.StatusCode == http.StatusPreconditionFailed {
		if r, err := daemon.DecodeRedirect(res.Header.Get(daemon.RedirectHeader)); err == nil {
			return hubMovedError{r}
		}
	}
	b, _ := io.ReadAll(io.LimitReader(res.Body, 64<<10))
	var e struct {
		Error   string `json:"error"`
		Message string `json:"message"`
	}
	msg := ""
	if json.Unmarshal(b, &e) == nil {
		msg = e.Error
		if msg == "" {
			msg = e.Message
		}
	}
	if msg == "" {
		msg = strings.TrimSpace(string(b))
	}
	if msg == "" {
		msg = res.Status
	}
	msg = cellText(msg)
	switch res.StatusCode {
	case http.StatusUnauthorized:
		return fmt.Errorf("authentication failed: %s (check your API key, or %s)", msg, loginHint)
	case http.StatusForbidden:
		return fmt.Errorf("permission denied: %s", msg)
	case http.StatusNotFound:
		return fmt.Errorf("not found: %s", msg)
	case http.StatusBadRequest, http.StatusRequestEntityTooLarge:
		return fmt.Errorf("rejected by hub: %s", msg)
	default:
		return fmt.Errorf("hub error (%s) at %s: %s", res.Status, hubURL, msg)
	}
}

// artifactRequest sends one request to the artifact endpoints with the API key.
func (hc *hubClient) artifactRequest(ctx context.Context, method, path string, body io.Reader, size int64) (*http.Response, error) {
	req, err := http.NewRequestWithContext(ctx, method, hc.url+path, body)
	if err != nil {
		return nil, err
	}
	if body != nil {
		req.ContentLength = size
		req.Header.Set("Content-Type", "application/octet-stream")
	}
	req.Header.Set("Authorization", hc.authz.header)
	res, err := hc.raw.Do(req)
	if err != nil {
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		return nil, fmt.Errorf("hub unreachable at %s: %w", hc.url, err)
	}
	if res.StatusCode < 200 || res.StatusCode > 299 {
		defer func() { _ = res.Body.Close() }()
		return nil, artifactHTTPError(hc.url, res)
	}
	return res, nil
}

type savedArtifactJSON struct {
	ID     string `json:"id"`
	Path   string `json:"path"`
	Size   int64  `json:"size"`
	SHA256 string `json:"sha256"`
}

func (a *app) artifactsGetCmd() *cobra.Command {
	var out string
	cmd := &cobra.Command{
		Use:   "get <id> [-o path]",
		Short: "Download an artifact (to -o, \"-\" for stdout, or to its name in the current directory)",
		Args:  exactArgs(1, "id"),
		RunE: func(cmd *cobra.Command, args []string) error {
			id := args[0]
			hc, err := a.hub(cmd)
			if err != nil {
				return err
			}
			// Ctrl-C cancels the download, so the temporary file is removed.
			ctx, stop := signal.NotifyContext(cmd.Context(), os.Interrupt, syscall.SIGTERM)
			defer stop()
			meta, err := hc.arts.GetArtifact(ctx, connect.NewRequest(&mycelv1.GetArtifactRequest{Id: id}))
			if err != nil {
				return rpcError(hc.url, err)
			}
			art := meta.Msg.GetArtifact()
			dest, replace := out, true
			if dest == "" {
				dest, replace = artifactFileName(art.GetName()), false
				if dest == "" {
					return fmt.Errorf("artifact %s has no usable file name; choose one with -o", cellText(id))
				}
				if _, err := os.Lstat(dest); err == nil {
					return fmt.Errorf("%s already exists; choose another path with -o", dest)
				}
			}
			res, err := hc.artifactRequest(ctx, http.MethodGet, "/api/artifacts/"+url.PathEscape(id), nil, 0)
			if err != nil {
				return err
			}
			defer func() { _ = res.Body.Close() }()
			limit := int64(maxArtifactUpload)
			if art.GetSize() > 0 {
				limit = art.GetSize()
			}
			body := io.LimitReader(res.Body, limit+1)
			h := sha256.New()
			copyBody := func(w io.Writer) (int64, error) {
				n, err := io.Copy(io.MultiWriter(w, h), body)
				if err != nil {
					return n, fmt.Errorf("download %s: %w", id, err)
				}
				if n > limit {
					return n, fmt.Errorf("download of %s is larger than its %d bytes", cellText(id), limit)
				}
				return n, checkDownload(art, n, h.Sum(nil))
			}
			var n int64
			if dest == "-" {
				_, err = copyBody(a.stdout)
				return err
			}
			n, err = streamToFile(dest, replace, copyBody)
			if err != nil {
				return err
			}
			sum := hex.EncodeToString(h.Sum(nil))
			if a.jsonOut {
				return a.printJSONValue(savedArtifactJSON{ID: id, Path: dest, Size: n, SHA256: sum})
			}
			_, err = fmt.Fprintf(a.stdout, "Saved %s to %s (%s).\n", cellText(id), dest, sizeCell(n))
			return err
		},
	}
	cmd.Flags().StringVarP(&out, "output", "o", "", "destination file; \"-\" writes to stdout")
	return cmd
}

// checkDownload compares what arrived with the hub's metadata.
func checkDownload(art *mycelv1.Artifact, n int64, sum []byte) error {
	if art.GetSize() > 0 && n != art.GetSize() {
		return fmt.Errorf("download of %s is incomplete: got %d of %d bytes", cellText(art.GetId()), n, art.GetSize())
	}
	if want := strings.ToLower(art.GetSha256()); want != "" && want != hex.EncodeToString(sum) {
		return fmt.Errorf("download of %s does not match its sha256 %s", cellText(art.GetId()), cellText(want))
	}
	return nil
}

// streamToFile writes what fill produces to a temporary file next to dest
// and publishes it on success: renamed over dest when replace is set,
// otherwise hard-linked to dest, which fails instead of replacing a file that
// appeared meanwhile.
func streamToFile(dest string, replace bool, fill func(io.Writer) (int64, error)) (int64, error) {
	dir := filepath.Dir(dest)
	var rnd [6]byte
	_, _ = rand.Read(rnd[:])
	tmp := filepath.Join(dir, ".mycel-get-"+hex.EncodeToString(rnd[:]))
	f, err := os.OpenFile(tmp, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o644) //nolint:gosec // path chosen by the user
	if err != nil {
		return 0, err
	}
	n, err := fill(f)
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	if err == nil {
		err = publishFile(tmp, dest, replace)
	}
	_ = os.Remove(tmp) // after a link or a failure; gone after a rename
	return n, err
}

func publishFile(tmp, dest string, replace bool) error {
	if replace {
		return os.Rename(tmp, dest)
	}
	err := os.Link(tmp, dest)
	switch {
	case err == nil:
		return nil
	case errors.Is(err, fs.ErrExist):
		return fmt.Errorf("%s already exists; choose another path with -o", dest)
	}
	// No hard links on this file system: rename if dest is still free.
	if _, lerr := os.Lstat(dest); lerr == nil {
		return fmt.Errorf("%s already exists; choose another path with -o", dest)
	}
	return os.Rename(tmp, dest)
}

func (a *app) artifactsPutCmd() *cobra.Command {
	var name, channel string
	cmd := &cobra.Command{
		Use:   "put <path> [--name n] [--channel name]",
		Short: "Upload a file as an artifact",
		Args:  exactArgs(1, "path"),
		RunE: func(cmd *cobra.Command, args []string) error {
			f, err := os.Open(args[0])
			if err != nil {
				return err
			}
			defer func() { _ = f.Close() }()
			st, err := f.Stat()
			if err != nil {
				return err
			}
			if !st.Mode().IsRegular() {
				return fmt.Errorf("%s is not a regular file", args[0])
			}
			if st.Size() > maxArtifactUpload {
				return fmt.Errorf("%s is %s, more than the 1 GiB an artifact may have", args[0], sizeCell(st.Size()))
			}
			if name == "" {
				name = filepath.Base(args[0])
			}
			q := url.Values{"name": {name}}
			if channel != "" {
				q.Set("channel", normChannel(channel))
			}
			hc, err := a.hub(cmd)
			if err != nil {
				return err
			}
			res, err := hc.artifactRequest(cmd.Context(), http.MethodPut, "/api/artifacts?"+q.Encode(), io.LimitReader(f, st.Size()), st.Size())
			if err != nil {
				return err
			}
			defer func() { _ = res.Body.Close() }()
			b, err := io.ReadAll(io.LimitReader(res.Body, 1<<20))
			if err != nil {
				return fmt.Errorf("read hub reply: %w", err)
			}
			var art mycelv1.Artifact
			if err := (protojson.UnmarshalOptions{DiscardUnknown: true}).Unmarshal(b, &art); err != nil {
				return fmt.Errorf("decode hub reply: %w", err)
			}
			if a.jsonOut {
				return a.printProto(&art)
			}
			_, err = fmt.Fprintf(a.stdout, "Uploaded %s as %s (%s, sha256 %s).\n", cellText(art.GetName()), cellText(art.GetId()),
				sizeCell(art.GetSize()), dash(cellText(art.GetSha256())))
			return err
		},
	}
	cmd.Flags().StringVar(&name, "name", "", "artifact name (default the file's base name)")
	cmd.Flags().StringVar(&channel, "channel", "", "attach the artifact to this channel so its members may fetch it")
	return cmd
}

func (a *app) artifactsRmCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "rm <id>",
		Short: "Delete an artifact",
		Args:  exactArgs(1, "id"),
		RunE: func(cmd *cobra.Command, args []string) error {
			hc, err := a.hub(cmd)
			if err != nil {
				return err
			}
			resp, err := hc.arts.DeleteArtifact(cmd.Context(), connect.NewRequest(&mycelv1.DeleteArtifactRequest{Id: args[0]}))
			if err != nil {
				return rpcError(hc.url, err)
			}
			if a.jsonOut {
				return a.printProto(resp.Msg)
			}
			_, err = fmt.Fprintf(a.stdout, "Deleted %s.\n", cellText(args[0]))
			return err
		},
	}
}
