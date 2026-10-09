package loadgen

import (
	"context"
	"fmt"
	"net"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	pionrtp "github.com/pion/rtp"

	"github.com/mharner33/voice-demo/internal/codec"
	"github.com/mharner33/voice-demo/internal/control"
	"github.com/mharner33/voice-demo/internal/rtp"
)

// testGateway is a gateway with the AI pipeline removed: a real control plane,
// a real UDP media port, and real per-SSRC RTP accounting.
//
// Those are exactly the parts a load run interacts with, and they are the real
// implementations rather than fakes — which is what lets these tests assert
// the thing that matters most about a load run: that the gateway independently
// measures the loss the client injected, on every one of fifty concurrent
// calls. A stub that echoed the client's own numbers back would assert
// nothing.
type testGateway struct {
	mediaPort int
	grpcAddr  net.Addr
	recv      *rtp.Receiver

	// startErr and endErr make the control plane fail, for the tests about how
	// a load run reports a gateway that will not cooperate.
	startErr error
	endErr   error

	// endDelay stalls teardown, standing in for a slow pipeline.
	endDelay time.Duration

	mu       sync.Mutex
	calls    map[string]*testCall
	bySSRC   map[uint32]*testCall
	starts   int
	nextID   int
	ended    int
	liveNow  int
	livePeak int
}

type testCall struct {
	id      string
	req     control.StartRequest
	report  control.SenderReport
	summary control.CallSummary
}

func newTestGateway(t *testing.T) *testGateway {
	t.Helper()

	mediaConn, err := net.ListenPacket("udp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listening for RTP: %v", err)
	}
	grpcLis, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listening for the control plane: %v", err)
	}

	gw := &testGateway{
		mediaPort: mediaConn.LocalAddr().(*net.UDPAddr).Port,
		grpcAddr:  grpcLis.Addr(),
		recv:      rtp.NewReceiver(mediaConn, codec.SampleRate8k),
		calls:     map[string]*testCall{},
		bySSRC:    map[uint32]*testCall{},
	}

	var packets atomic.Uint64
	gw.recv.OnPacket(func(_ *rtp.Session, _ *pionrtp.Packet, _ net.Addr, _ time.Time) {
		packets.Add(1)
	})

	ctx, cancel := context.WithCancel(context.Background())
	var wg sync.WaitGroup

	wg.Add(1)
	go func() {
		defer wg.Done()
		_ = gw.recv.Serve(ctx)
	}()
	wg.Add(1)
	go func() {
		defer wg.Done()
		_ = control.Serve(ctx, grpcLis, gw)
	}()

	t.Cleanup(func() {
		cancel()
		wg.Wait()
		mediaConn.Close()
	})

	return gw
}

// controlAddr is recorded at construction rather than asked for later,
// because control.Serve takes ownership of the listener.
func (g *testGateway) controlAddr(t *testing.T) string {
	t.Helper()
	if g.grpcAddr == nil {
		t.Fatal("the control plane has no address")
	}
	return g.grpcAddr.String()
}

func (g *testGateway) StartCall(_ context.Context, req control.StartRequest) (control.StartResult, error) {
	g.mu.Lock()
	defer g.mu.Unlock()

	g.starts++
	if g.startErr != nil {
		return control.StartResult{}, g.startErr
	}

	g.nextID++
	id := fmt.Sprintf("c-%04d", g.nextID)
	c := &testCall{id: id, req: req}
	g.calls[id] = c
	g.bySSRC[req.SSRC] = c

	g.liveNow++
	if g.liveNow > g.livePeak {
		g.livePeak = g.liveNow
	}

	return control.StartResult{
		CallID:    id,
		MediaPort: g.mediaPort,
		Codec:     req.Codec,
		TraceID:   "trace-" + id,
	}, nil
}

func (g *testGateway) EndCall(ctx context.Context, callID string, report control.SenderReport) (
	control.CallSummary, error) {

	if g.endDelay > 0 {
		select {
		case <-ctx.Done():
			return control.CallSummary{}, ctx.Err()
		case <-time.After(g.endDelay):
		}
	}

	g.mu.Lock()
	c := g.calls[callID]
	endErr := g.endErr
	g.mu.Unlock()

	if c == nil {
		return control.CallSummary{}, control.ErrCallNotFound
	}
	if endErr != nil {
		return control.CallSummary{}, endErr
	}

	// The sequence range is applied exactly as the real gateway does, so loss
	// at the edges of the stream is countable. Without it a stream whose last
	// packets were dropped under-reports, which is the whole reason the
	// control plane carries the range.
	sess, ok := g.recv.Session(c.req.SSRC)
	if !ok {
		return control.CallSummary{}, fmt.Errorf("no RTP stream for ssrc %#08x", c.req.SSRC)
	}
	if report.HaveRange {
		sess.NoteSequenceRange(report.FirstSequence, report.FinalSequence)
	}
	st := sess.Stats()

	sum := control.CallSummary{
		PacketsReceived: st.Received,
		PacketsExpected: st.Expected,
		PacketsLost:     st.Lost,
		LossPct:         st.LossPct,
		JitterMs:        st.JitterMs,
		MOS:             st.MOS(),
		Turns:           1,
		ToolCalls:       1,
		Transcripts:     []string{"hello I'm calling about my account balance"},
		Replies:         []string{"I've pulled up your account."},
		TraceID:         "trace-" + callID,
	}

	g.mu.Lock()
	c.report = report
	c.summary = sum
	g.ended++
	g.liveNow--
	g.mu.Unlock()

	return sum, nil
}

func (g *testGateway) counts() (starts, ended, peak int) {
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.starts, g.ended, g.livePeak
}

func (g *testGateway) startRequests() []control.StartRequest {
	g.mu.Lock()
	defer g.mu.Unlock()
	out := make([]control.StartRequest, 0, len(g.calls))
	for _, c := range g.calls {
		out = append(out, c.req)
	}
	return out
}
