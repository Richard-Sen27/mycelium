package daemon

import (
	"context"
	"errors"
	"time"

	"github.com/nats-io/nats.go"
	"google.golang.org/protobuf/proto"

	mycelv1 "github.com/richard-sen27/mycelium/gen/mycel/v1"
	"github.com/richard-sen27/mycelium/internal/bus"
)

// Errors of hubRPC that are not answers from the hub.
var (
	errBusDown     = errors.New("hub not connected")
	errNoResponder = errors.New("hub is not serving this method")
	errHubTimeout  = errors.New("hub did not answer in time")
	errBadReply    = errors.New("malformed reply from hub")
)

// hubError is a refusal from the hub (Mycel-Status: error). msg is the hub's
// CtlResponse.error text.
type hubError struct{ msg string }

func (e *hubError) Error() string { return e.msg }

// busConn returns the current bus connection if it is up.
func (n *node) busConn() *nats.Conn {
	n.mu.Lock()
	defer n.mu.Unlock()
	if n.closed || n.nc == nil || !n.nc.IsConnected() {
		return nil
	}
	return n.nc
}

// waitBus waits up to d for the bus connection to come up.
func (n *node) waitBus(ctx context.Context, d time.Duration) *nats.Conn {
	deadline := time.Now().Add(d)
	for {
		if nc := n.busConn(); nc != nil {
			return nc
		}
		if !time.Now().Before(deadline) {
			return nil
		}
		t := time.NewTimer(min(50*time.Millisecond, time.Until(deadline)))
		select {
		case <-ctx.Done():
			t.Stop()
			return nil
		case <-t.C:
		}
	}
}

// hubRPC sends req on bus.NodeRPC(node, method) and decodes the reply into
// resp following the Mycel-Status convention (D014). Errors never contain
// the request payload, so agent tokens cannot leak through them.
func (n *node) hubRPC(ctx context.Context, method string, req, resp proto.Message) error {
	nc := n.busConn()
	if nc == nil {
		return errBusDown
	}
	b, err := proto.Marshal(req)
	if err != nil {
		return errors.New("encode request")
	}
	msg := nats.NewMsg(bus.NodeRPC(n.name(), method))
	msg.Data = b
	reply, err := nc.RequestMsgWithContext(ctx, msg)
	if err != nil {
		switch {
		case errors.Is(err, nats.ErrNoResponders):
			return errNoResponder
		case errors.Is(err, context.DeadlineExceeded), errors.Is(err, nats.ErrTimeout):
			if ctx.Err() != nil && !errors.Is(ctx.Err(), context.DeadlineExceeded) {
				return ctx.Err()
			}
			return errHubTimeout
		case errors.Is(err, context.Canceled):
			return err
		default:
			return errBusDown
		}
	}
	switch reply.Header.Get(bus.StatusHeader) {
	case bus.StatusOK:
		if err := proto.Unmarshal(reply.Data, resp); err != nil {
			return errBadReply
		}
		return nil
	case bus.StatusError:
		cr := &mycelv1.CtlResponse{}
		if err := proto.Unmarshal(reply.Data, cr); err != nil {
			return errBadReply
		}
		if cr.GetError() == "" {
			return &hubError{msg: "hub refused the request"}
		}
		n.redirectFromReply(reply.Header, cr.GetError())
		return &hubError{msg: cr.GetError()}
	default:
		return errBadReply
	}
}
