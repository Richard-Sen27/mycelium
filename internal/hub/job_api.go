package hub

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"os"
	"path/filepath"

	"connectrpc.com/connect"

	mycelv1 "github.com/richard-sen27/mycelium/gen/mycel/v1"
	"github.com/richard-sen27/mycelium/gen/mycel/v1/mycelv1connect"
	"github.com/richard-sen27/mycelium/internal/policy"
)

// ---- JobService ----

type jobService struct{ s *server }

var _ mycelv1connect.JobServiceHandler = (*jobService)(nil)

// logStreamDeadlines hands StreamLogs its response controller (see
// streamDeadlines).
func logStreamDeadlines(h http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == mycelv1connect.JobServiceStreamLogsProcedure {
			r = r.WithContext(context.WithValue(r.Context(), ctxStream, http.NewResponseController(w)))
		}
		h.ServeHTTP(w, r)
	})
}

func (js *jobService) user(ctx context.Context) (actor, error) {
	u, err := requireUser(ctx)
	if err != nil {
		return actor{}, err
	}
	return userActor(u), nil
}

func (js *jobService) RunJob(ctx context.Context, req *connect.Request[mycelv1.RunJobRequest]) (*connect.Response[mycelv1.RunJobResponse], error) {
	a, err := js.user(ctx)
	if err != nil {
		return nil, err
	}
	job, err := js.s.submitJob(ctx, a, req.Msg.GetSpec())
	if err != nil {
		return nil, err
	}
	return connect.NewResponse(&mycelv1.RunJobResponse{Job: job}), nil
}

func (js *jobService) GetJob(ctx context.Context, req *connect.Request[mycelv1.GetJobRequest]) (*connect.Response[mycelv1.GetJobResponse], error) {
	a, err := js.user(ctx)
	if err != nil {
		return nil, err
	}
	j, err := js.s.loadJob(ctx, a, policy.ActionJobGet, req.Msg.GetId())
	if err != nil {
		return nil, err
	}
	job, err := js.s.jobProto(ctx, j)
	if err != nil {
		return nil, err
	}
	return connect.NewResponse(&mycelv1.GetJobResponse{Job: job}), nil
}

func (js *jobService) ListJobs(ctx context.Context, req *connect.Request[mycelv1.ListJobsRequest]) (*connect.Response[mycelv1.ListJobsResponse], error) {
	s := js.s
	a, err := js.user(ctx)
	if err != nil {
		return nil, err
	}
	if err := s.agentOrUserAllowed(ctx, a, policy.ActionJobList); err != nil {
		return nil, err
	}
	owner := a.p.ID
	if a.isAdmin() {
		owner = ""
	}
	rows, err := s.st.listJobs(ctx, owner, req.Msg.GetIncludeFinished(), clampLimit(req.Msg.GetLimit(), defaultList, maxList))
	if err != nil {
		return nil, s.internalError("list jobs", err)
	}
	out := &mycelv1.ListJobsResponse{}
	for _, r := range rows {
		j, err := s.jobProto(ctx, r)
		if err != nil {
			return nil, err
		}
		out.Jobs = append(out.Jobs, j)
	}
	return connect.NewResponse(out), nil
}

func (js *jobService) CancelJob(ctx context.Context, req *connect.Request[mycelv1.CancelJobRequest]) (*connect.Response[mycelv1.CancelJobResponse], error) {
	a, err := js.user(ctx)
	if err != nil {
		return nil, err
	}
	job, err := js.s.cancelJob(ctx, a, req.Msg.GetId())
	if err != nil {
		return nil, err
	}
	return connect.NewResponse(&mycelv1.CancelJobResponse{Job: job}), nil
}

// StreamLogs sends the job's log from from_byte in 64 KiB chunks; with
// follow it then waits for more until the job finishes. The last message
// has eof set once the job is finished and everything was sent.
func (js *jobService) StreamLogs(ctx context.Context, req *connect.Request[mycelv1.StreamLogsRequest], stream *connect.ServerStream[mycelv1.StreamLogsResponse]) error {
	s := js.s
	a, err := js.user(ctx)
	if err != nil {
		return err
	}
	j, err := s.loadJob(ctx, a, policy.ActionJobLogs, req.Msg.GetId())
	if err != nil {
		return err
	}
	off := req.Msg.GetFromByte()
	if off > j.LogBytes {
		return invalidArg("from_byte %d is beyond the end of the log (%d bytes)", off, j.LogBytes)
	}
	if req.Msg.GetFollow() {
		if !s.acquireLogStream(a.p.ID) {
			return connect.NewError(connect.CodeResourceExhausted, errors.New("too many log streams"))
		}
		defer s.releaseLogStream(a.p.ID)
	}
	// A long log or a slow reader may outlast the server's timeouts.
	liftStreamDeadlines(ctx)
	if req.Msg.GetFollow() {
		// Send the headers now: a client waits for them before reading.
		if err := stream.Send(nil); err != nil {
			return err
		}
	}
	for {
		woke, unsub := s.ch.notify.subscribe(jobKey(j.ID))
		j, err = s.st.getJob(ctx, j.ID)
		if err != nil {
			unsub()
			return s.internalError("get job", err)
		}
		for off < j.LogBytes {
			data, err := s.readLog(j.ID, off, int(min(uint64(streamLogChunk), j.LogBytes-off))) //nolint:gosec // at most streamLogChunk
			if err != nil {
				unsub()
				return s.internalError("read log", err)
			}
			if len(data) == 0 {
				break
			}
			if err := stream.Send(&mycelv1.StreamLogsResponse{Data: data, Offset: off}); err != nil {
				unsub()
				return err
			}
			off += uint64(len(data))
		}
		if j.finished() {
			unsub()
			return stream.Send(&mycelv1.StreamLogsResponse{Offset: off, Eof: true})
		}
		if !req.Msg.GetFollow() {
			unsub()
			return nil
		}
		select {
		case <-woke:
			unsub()
		case <-ctx.Done():
			unsub()
			return ctx.Err()
		case <-s.ch.stop:
			unsub()
			return connect.NewError(connect.CodeUnavailable, errors.New("hub stopping"))
		}
	}
}

func (s *server) acquireLogStream(user string) bool {
	s.jobs.smu.Lock()
	defer s.jobs.smu.Unlock()
	if s.jobs.logStreams[user] >= maxLogStreamsPerUser {
		return false
	}
	s.jobs.logStreams[user]++
	return true
}

func (s *server) releaseLogStream(user string) {
	s.jobs.smu.Lock()
	defer s.jobs.smu.Unlock()
	if s.jobs.logStreams[user]--; s.jobs.logStreams[user] <= 0 {
		delete(s.jobs.logStreams, user)
	}
}

func (js *jobService) ListProfiles(ctx context.Context, _ *connect.Request[mycelv1.ListProfilesRequest]) (*connect.Response[mycelv1.ListProfilesResponse], error) {
	s := js.s
	a, err := js.user(ctx)
	if err != nil {
		return nil, err
	}
	if err := s.agentOrUserAllowed(ctx, a, policy.ActionProfileList); err != nil {
		return nil, err
	}
	ps, err := s.profiles(ctx)
	if err != nil {
		return nil, s.internalError("profiles", err)
	}
	return connect.NewResponse(&mycelv1.ListProfilesResponse{Profiles: ps}), nil
}

func (js *jobService) ListNodesForProfile(ctx context.Context, req *connect.Request[mycelv1.ListNodesForProfileRequest]) (*connect.Response[mycelv1.ListNodesForProfileResponse], error) {
	s := js.s
	a, err := js.user(ctx)
	if err != nil {
		return nil, err
	}
	if err := s.agentOrUserAllowed(ctx, a, policy.ActionProfileList); err != nil {
		return nil, err
	}
	prof, err := s.profile(ctx, req.Msg.GetProfile())
	if errors.Is(err, errNotFound) {
		return nil, notFound("profile")
	}
	if err != nil {
		return nil, s.internalError("profile", err)
	}
	cands, err := s.onlineCandidates(ctx)
	if err != nil {
		return nil, s.internalError("nodes", err)
	}
	out := &mycelv1.ListNodesForProfileResponse{Nodes: []string{}}
	for _, c := range cands {
		if ok, _ := nodeFits(c, prof); ok {
			out.Nodes = append(out.Nodes, c.name)
		}
	}
	return connect.NewResponse(out), nil
}

// ---- ArtifactService ----

type artifactService struct{ s *server }

var _ mycelv1connect.ArtifactServiceHandler = (*artifactService)(nil)

func (as *artifactService) ListArtifacts(ctx context.Context, req *connect.Request[mycelv1.ListArtifactsRequest]) (*connect.Response[mycelv1.ListArtifactsResponse], error) {
	u, err := requireUser(ctx)
	if err != nil {
		return nil, err
	}
	arts, err := as.s.visibleArtifacts(ctx, userActor(u), req.Msg)
	if err != nil {
		return nil, err
	}
	return connect.NewResponse(&mycelv1.ListArtifactsResponse{Artifacts: arts}), nil
}

// loadArtifact loads an artifact and checks action on it for a.
func (s *server) loadArtifact(ctx context.Context, a actor, action policy.Action, id string) (*artifactRow, error) {
	if !validArtifactID(id) {
		return nil, invalidArg("invalid artifact id %q", id)
	}
	art, err := s.st.getArtifact(ctx, id)
	if errors.Is(err, errNotFound) {
		return nil, notFound("artifact")
	}
	if err != nil {
		return nil, s.internalError("get artifact", err)
	}
	// An artifact the caller may not see is reported missing.
	d, err := s.artifactDecision(ctx, a, policy.ActionArtifactGet, art)
	if err != nil {
		return nil, err
	}
	if !d.Allow {
		s.auditDenied(ctx, a.auditID(), string(action), "artifact:"+id, map[string]any{"reason": d.Reason})
		return nil, notFound("artifact")
	}
	if action != policy.ActionArtifactGet {
		if d, err = s.artifactDecision(ctx, a, action, art); err != nil {
			return nil, err
		}
		if !d.Allow {
			s.auditDenied(ctx, a.auditID(), string(action), "artifact:"+id, map[string]any{"reason": d.Reason})
			return nil, connect.NewError(connect.CodePermissionDenied, errors.New(d.Reason))
		}
	}
	return art, nil
}

func (as *artifactService) GetArtifact(ctx context.Context, req *connect.Request[mycelv1.GetArtifactRequest]) (*connect.Response[mycelv1.GetArtifactResponse], error) {
	u, err := requireUser(ctx)
	if err != nil {
		return nil, err
	}
	art, err := as.s.loadArtifact(ctx, userActor(u), policy.ActionArtifactGet, req.Msg.GetId())
	if err != nil {
		return nil, err
	}
	return connect.NewResponse(&mycelv1.GetArtifactResponse{Artifact: art.toProto()}), nil
}

func (as *artifactService) DeleteArtifact(ctx context.Context, req *connect.Request[mycelv1.DeleteArtifactRequest]) (*connect.Response[mycelv1.DeleteArtifactResponse], error) {
	s := as.s
	u, err := requireUser(ctx)
	if err != nil {
		return nil, err
	}
	art, err := s.loadArtifact(ctx, userActor(u), policy.ActionArtifactDelete, req.Msg.GetId())
	if err != nil {
		return nil, err
	}
	active, err := s.st.unfinishedJobs(ctx)
	if err != nil {
		return nil, s.internalError("unfinished jobs", err)
	}
	for _, j := range active {
		if specUses(j.Spec, art.ID) {
			return nil, connect.NewError(connect.CodeFailedPrecondition, fmt.Errorf("artifact is an input of unfinished job %s", j.ID))
		}
	}
	if err := s.st.deleteArtifact(ctx, art.ID); err != nil && !errors.Is(err, errNotFound) {
		return nil, s.internalError("delete artifact", err)
	}
	if err := os.Remove(filepath.Join(s.dir, art.Path)); err != nil && !errors.Is(err, os.ErrNotExist) {
		s.log.Warn("remove artifact file", "artifact", art.ID, "err", err)
	}
	s.audit(ctx, u.ID, string(policy.ActionArtifactDelete), "artifact:"+art.ID, outcomeOK, map[string]any{"owner_id": art.OwnerID, "size": art.Size})
	return connect.NewResponse(&mycelv1.DeleteArtifactResponse{}), nil
}
