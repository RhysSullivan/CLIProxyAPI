package openai

import "testing"

func TestHTTPInterruptBindsToCurrentResponseAndRetainsTerminalIDs(t *testing.T) {
	state := newResponsesLocalInterrupt()
	state.begin()
	state.observe([]byte(`{"type":"response.created","response":{"id":"first"}}`))
	state.observe([]byte(`{"type":"response.completed","response":{"id":"first"}}`))
	state.end()
	state.begin()
	state.observe([]byte(`{"type":"response.created","response":{"id":"second"}}`))
	stale := []byte(`{"type":"response.interrupt","response_id":"first"}`)
	if !state.alreadyTerminal(stale) {
		t.Fatal("completed HTTP response was forgotten")
	}
	if state.deliver(stale) {
		t.Fatal("late interrupt canceled a different HTTP response")
	}
	if state.deliver([]byte(`{"type":"response.interrupt","response_id":"unknown"}`)) {
		t.Fatal("unknown response interrupted active HTTP response")
	}
	active := []byte(`{"type":"response.interrupt","response_id":"second"}`)
	if !state.deliver(active) {
		t.Fatal("active HTTP response could not be interrupted")
	}
	<-state.framesChan()
	state.finish(active)
	if !state.alreadyTerminal(active) {
		t.Fatal("locally interrupted response was forgotten")
	}
	state.end()
	state.begin()
	state.observe([]byte(`{"type":"response.created","response":{"id":"third"}}`))
	if state.deliver(active) {
		t.Fatal("duplicate interrupt canceled the following HTTP response")
	}
	select {
	case <-state.framesChan():
		t.Fatal("stale interrupt leaked into following turn")
	default:
	}
}
