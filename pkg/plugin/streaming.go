package plugin

import (
	"context"
	"errors"
	"fmt"
	"sync"

	"github.com/google/uuid"

	"github.com/opentalon/opentalon/proto/pluginpb"
)

// HostCaller is the synchronous host-side interface a StreamingHandler
// receives via Execute's ctx. It calls back into the host's
// orchestrator (its expert system) and returns the result the way
// the LLM would see it.
type HostCaller interface {
	// RunAction asks the host to dispatch plugin.action(args) through
	// its normal executeCall path. Returns the action's text content
	// (and any structured payload), or an error.
	RunAction(ctx context.Context, plugin, action string, args map[string]string) (CallResult, error)
}

// CallResult is the host's reply to a RunAction call.
type CallResult struct {
	Content           string
	StructuredContent string // raw JSON, empty when the upstream action didn't emit one
}

// StreamingHandler is the plugin-side interface for actions that need
// to call back into the host orchestrator mid-execution. Implement it
// alongside Handler, then set CapabilitiesMsg.SupportsCallbacks = true.
// The host will then dispatch this plugin's actions over ExecuteBidi
// and pass a live HostCaller to ExecuteWithCallbacks.
//
// Plugins that don't need callbacks should NOT implement this
// interface — leaving SupportsCallbacks=false keeps the unary Execute
// path and avoids the streaming overhead.
type StreamingHandler interface {
	Handler // Capabilities + Execute (the latter may panic or be a thin redirect when SupportsCallbacks=true; the host won't call Execute on such plugins)
	ExecuteWithCallbacks(ctx context.Context, req Request, host HostCaller) Response
}

// hostCallerStream is the HostCaller the SDK hands to a
// StreamingHandler. It serialises callback round-trips over the
// underlying gRPC stream — only one in-flight RunAction at a time,
// which matches the synchronous mental model plugin authors expect.
type hostCallerStream struct {
	stream pluginpb.PluginService_ExecuteBidiServer

	// mu guards both send order (gRPC streams allow only one Send at
	// a time) and the inflight map.
	mu       sync.Mutex
	inflight map[string]chan *pluginpb.CallbackResponse
}

func newHostCallerStream(stream pluginpb.PluginService_ExecuteBidiServer) *hostCallerStream {
	return &hostCallerStream{
		stream:   stream,
		inflight: make(map[string]chan *pluginpb.CallbackResponse),
	}
}

// RunAction emits a CallbackRequest on the stream, blocks until the
// matching CallbackResponse arrives, and returns it. Concurrent calls
// from the same StreamingHandler are serialised by mu — gRPC streams
// can't safely interleave Sends, and the receive loop has to dispatch
// responses to the right waiter anyway.
func (h *hostCallerStream) RunAction(ctx context.Context, plugin, action string, args map[string]string) (CallResult, error) {
	id := uuid.NewString()
	wait := make(chan *pluginpb.CallbackResponse, 1)

	h.mu.Lock()
	h.inflight[id] = wait
	err := h.stream.Send(&pluginpb.PluginMessage{
		Payload: &pluginpb.PluginMessage_CallbackRequest{
			CallbackRequest: &pluginpb.CallbackRequest{
				Id:     id,
				Plugin: plugin,
				Action: action,
				Args:   args,
			},
		},
	})
	if err != nil {
		delete(h.inflight, id)
		h.mu.Unlock()
		return CallResult{}, fmt.Errorf("send callback: %w", err)
	}
	h.mu.Unlock()

	select {
	case resp := <-wait:
		if resp.GetError() != "" {
			return CallResult{}, errors.New(resp.GetError())
		}
		return CallResult{
			Content:           resp.GetContent(),
			StructuredContent: resp.GetStructuredContent(),
		}, nil
	case <-ctx.Done():
		h.mu.Lock()
		delete(h.inflight, id)
		h.mu.Unlock()
		return CallResult{}, ctx.Err()
	}
}

// deliverResponse hands an incoming CallbackResponse to the goroutine
// blocked in RunAction. Called from the stream's receive loop.
func (h *hostCallerStream) deliverResponse(resp *pluginpb.CallbackResponse) {
	h.mu.Lock()
	wait, ok := h.inflight[resp.GetId()]
	if ok {
		delete(h.inflight, resp.GetId())
	}
	h.mu.Unlock()
	if !ok {
		// Unknown id — host bug or stale frame after timeout. Dropping
		// is correct; the RunAction caller has already returned via
		// ctx.Done().
		return
	}
	wait <- resp
}

// ExecuteBidi is the server-side implementation of the bidirectional
// streaming RPC. It is registered when the Handler also implements
// StreamingHandler and the plugin's Capabilities reports
// SupportsCallbacks = true. The host calls this in place of unary
// Execute for matching plugins.
//
// Frame flow:
//
//  1. Host sends HostMessage{call} — the tool call request.
//  2. SDK starts the StreamingHandler in a goroutine, passing a
//     hostCallerStream that the handler uses for callbacks.
//  3. Receive loop reads HostMessage{callback_response} frames and
//     dispatches them by id to whichever RunAction is waiting.
//  4. When the handler returns, send PluginMessage{result} and close
//     the send half.
func (s *grpcServer) ExecuteBidi(stream pluginpb.PluginService_ExecuteBidiServer) error {
	sh, ok := s.handler.(StreamingHandler)
	if !ok {
		return fmt.Errorf("plugin does not implement StreamingHandler")
	}

	first, err := stream.Recv()
	if err != nil {
		return fmt.Errorf("recv initial: %w", err)
	}
	callMsg := first.GetCall()
	if callMsg == nil {
		return fmt.Errorf("first message must be {call}; got %T", first.GetPayload())
	}

	host := newHostCallerStream(stream)

	// Handler runs on its own goroutine so the receive loop can keep
	// reading inbound CallbackResponse frames in parallel.
	type handlerResult struct {
		resp Response
		err  error
	}
	done := make(chan handlerResult, 1)
	go func() {
		defer func() {
			if r := recover(); r != nil {
				done <- handlerResult{err: fmt.Errorf("plugin panicked: %v", r)}
			}
		}()
		req := requestFromProto(callMsg)
		resp := sh.ExecuteWithCallbacks(stream.Context(), req, host)
		done <- handlerResult{resp: resp}
	}()

	// Run Recv on its own goroutine too so we can interleave it with
	// the handler's done signal in one select. A handler that returns
	// without firing any callback (or after firing some) must not
	// leave us blocked in stream.Recv() waiting for a message that
	// never comes.
	type recvFrame struct {
		hm  *pluginpb.HostMessage
		err error
	}
	recvCh := make(chan recvFrame, 1)
	recvCtx, cancelRecv := context.WithCancel(stream.Context())
	defer cancelRecv()
	go func() {
		for {
			hm, err := stream.Recv()
			select {
			case recvCh <- recvFrame{hm: hm, err: err}:
			case <-recvCtx.Done():
				return
			}
			if err != nil {
				return
			}
		}
	}()

	for {
		select {
		case res := <-done:
			if res.err != nil {
				return res.err
			}
			return stream.Send(&pluginpb.PluginMessage{
				Payload: &pluginpb.PluginMessage_Result{
					Result: responseToProto(res.resp),
				},
			})
		case rf := <-recvCh:
			if rf.err != nil {
				// Stream closed by client (e.g. host cancelled). Wait
				// for the handler to finish to keep its goroutine from
				// leaking, then return the error.
				<-done
				return rf.err
			}
			switch payload := rf.hm.GetPayload().(type) {
			case *pluginpb.HostMessage_CallbackResponse:
				host.deliverResponse(payload.CallbackResponse)
			case *pluginpb.HostMessage_Call:
				return fmt.Errorf("unexpected {call} mid-stream")
			default:
				return fmt.Errorf("unknown HostMessage payload %T", payload)
			}
		}
	}
}
