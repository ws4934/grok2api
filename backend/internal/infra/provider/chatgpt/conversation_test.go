package chatgpt

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/chenyme/grok2api/backend/internal/domain/account"
	"github.com/chenyme/grok2api/backend/internal/infra/provider"
)

// turnEnd closes a reply the way ChatGPT does: the last token marker names the
// message a follow-up continues from.
const turnEnd = `data: {"type":"message_marker","conversation_id":"conv-1","message_id":"msg-end","marker":"last_token","event":"last"}` + "\n\n" +
	`data: {"type":"message_stream_complete","conversation_id":"conv-1"}` + "\n\ndata: [DONE]\n\n"

// conversationUpstream answers each conversation with stream, or fails it with
// the status reject returns, and records every payload.
func conversationUpstream(t *testing.T, payloads chan<- map[string]any, stream string, reject func(map[string]any) int) *httptest.Server {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if serveWebBasics(w, r) {
			return
		}
		var payload map[string]any
		_ = json.NewDecoder(r.Body).Decode(&payload)
		payloads <- payload
		if status := reject(payload); status != 0 {
			w.WriteHeader(status)
			_, _ = w.Write([]byte(`{"detail":"Conversation not found"}`))
			return
		}
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = w.Write([]byte(stream))
	}))
	t.Cleanup(server.Close)
	return server
}

func forwardSession(t *testing.T, a *Adapter, credential account.Credential, session, body string) map[string]any {
	t.Helper()
	response, err := a.ForwardResponse(context.Background(), provider.ResponseResourceRequest{Credential: credential, Method: http.MethodPost, Path: "/responses", Model: "gpt-5", PromptCacheKey: session, Body: []byte(body), NormalizeBody: true, Operation: "responses"})
	if err != nil {
		t.Fatal(err)
	}
	if response.StatusCode != http.StatusOK {
		t.Fatalf("status = %d", response.StatusCode)
	}
	return decodeBody(t, response)
}

// followUp is the client's next request: the history, the reply as an agent
// runtime echoes it (an empty text message, re-encoded arguments) and the
// tool result.
func followUp(t *testing.T, first map[string]any) string {
	t.Helper()
	call := outputOf(first, "function_call")[0]
	callID, _ := json.Marshal(call["call_id"])
	return `{"model":"gpt-5","tools":` + lookupTool + `,"input":[{"role":"user","content":"find x"},` +
		`{"role":"assistant","content":""},{"role":"assistant","content":[{"type":"output_text","text":"Sure."}]},` +
		`{"type":"function_call","call_id":` + string(callID) + `,"name":"lookup","arguments":"{ \"q\": \"<title>x</title>\" }"},` +
		`{"type":"function_call_output","call_id":` + string(callID) + `,"output":"found it"}]}`
}

const firstTurn = `{"model":"gpt-5","tools":` + lookupTool + `,"input":[{"role":"user","content":"find x"}]}`

func TestFollowUpContinuesTheConversation(t *testing.T) {
	payloads := make(chan map[string]any, 3)
	a, _ := newTestAdapter(t, conversationUpstream(t, payloads, answerSnapshots(callAnswer, "{}")+turnEnd, func(map[string]any) int { return 0 }))
	credential := testCredential(t, a, credentials{AccessToken: "AT"})
	first := forwardSession(t, a, credential, "session-1", firstTurn)
	if p := <-payloads; p["conversation_id"] != nil {
		t.Fatalf("first turn continued a conversation: %v", p["conversation_id"])
	}
	forwardSession(t, a, credential, "session-1", followUp(t, first))
	p := <-payloads
	messages := p["messages"].([]any)
	if p["conversation_id"] != "conv-1" || p["parent_message_id"] != "msg-end" || len(messages) != 1 || !strings.Contains(strings.Join(stringsOf(payloadParts(p, 0)), " "), "found it") {
		t.Fatalf("follow-up = conversation %v parent %v, %d messages", p["conversation_id"], p["parent_message_id"], len(messages))
	}
	// Another account cannot continue it: the whole history goes there.
	other := credential
	other.ID = 8
	forwardSession(t, a, other, "session-1", followUp(t, first))
	if p := <-payloads; p["conversation_id"] != nil || len(p["messages"].([]any)) < 4 {
		t.Fatalf("other account = conversation %v, %d messages", p["conversation_id"], len(p["messages"].([]any)))
	}
}

func TestRejectedContinuationResendsTheHistory(t *testing.T) {
	payloads := make(chan map[string]any, 3)
	reject := func(p map[string]any) int {
		if p["conversation_id"] != nil {
			return http.StatusNotFound
		}
		return 0
	}
	a, _ := newTestAdapter(t, conversationUpstream(t, payloads, answerSnapshots(callAnswer, "{}")+turnEnd, reject))
	credential := testCredential(t, a, credentials{AccessToken: "AT"})
	first := forwardSession(t, a, credential, "session-1", firstTurn)
	<-payloads
	object := forwardSession(t, a, credential, "session-1", followUp(t, first))
	if p := <-payloads; p["conversation_id"] != "conv-1" {
		t.Fatalf("expected a continuation first, got %v", p["conversation_id"])
	}
	if p := <-payloads; p["conversation_id"] != nil || len(p["messages"].([]any)) < 4 {
		t.Fatalf("fallback = conversation %v, %d messages", p["conversation_id"], len(p["messages"].([]any)))
	}
	if len(outputOf(object, "function_call")) != 1 {
		t.Fatalf("fallback answer = %v", object["output"])
	}
}

func TestNoSessionSendsTheWholeHistory(t *testing.T) {
	payloads := make(chan map[string]any, 2)
	a, _ := newTestAdapter(t, conversationUpstream(t, payloads, answerSnapshots(callAnswer, "{}")+turnEnd, func(map[string]any) int { return 0 }))
	credential := testCredential(t, a, credentials{AccessToken: "AT"})
	first := forwardSession(t, a, credential, "", firstTurn)
	<-payloads
	forwardSession(t, a, credential, "", followUp(t, first))
	if p := <-payloads; p["conversation_id"] != nil {
		t.Fatalf("continued without a session: %v", p["conversation_id"])
	}
}

func TestTooLargeConversationIsContextLengthExceeded(t *testing.T) {
	payloads := make(chan map[string]any, 1)
	a, _ := newTestAdapter(t, conversationUpstream(t, payloads, "", func(map[string]any) int { return http.StatusRequestEntityTooLarge }))
	response := forward(t, a, testCredential(t, a, credentials{AccessToken: "AT"}), "responses", firstTurn, false)
	body := decodeBody(t, response)
	if response.StatusCode != http.StatusBadRequest || body["error"].(map[string]any)["code"] != "context_length_exceeded" {
		t.Fatalf("413 = %d %v", response.StatusCode, body)
	}
}

// execAttempt is a call the model sent to ChatGPT's own tool channel; its
// content never reaches the stream.
const execAttempt = `data: {"message":{"id":"exec","author":{"role":"assistant"},"recipient":"functions.exec","channel":"commentary","status":"finished_successfully","content":{"content_type":"text","parts":[""]},"metadata":{"is_visually_hidden_from_conversation":true}}}` + "\n\n"

func lostCallUpstream(t *testing.T, payloads chan<- map[string]any, first, ask string) *httptest.Server {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if serveWebBasics(w, r) {
			return
		}
		var payload map[string]any
		_ = json.NewDecoder(r.Body).Decode(&payload)
		payloads <- payload
		w.Header().Set("Content-Type", "text/event-stream")
		if payload["conversation_id"] == nil {
			_, _ = w.Write([]byte(first))
			return
		}
		_, _ = w.Write([]byte(ask))
	}))
	t.Cleanup(server.Close)
	return server
}

func TestLostCallIsAskedForOnceInTheSameConversation(t *testing.T) {
	payloads := make(chan map[string]any, 3)
	first := answerSnapshots("I will look it up first.", "{}") + execAttempt + turnEnd
	ask := answerSnapshots(`Sorry.<tool_calls><tool_call><tool_name>lookup</tool_name><parameters>{"q":"x"}</parameters></tool_call></tool_calls>`, "{}") + turnEnd
	a, _ := newTestAdapter(t, lostCallUpstream(t, payloads, first, ask))
	object := forwardSession(t, a, testCredential(t, a, credentials{AccessToken: "AT"}), "", firstTurn)
	if len(payloads) != 2 {
		t.Fatalf("upstream calls = %d, want the reply and one ask", len(payloads))
	}
	<-payloads
	p := <-payloads
	notice := strings.Join(stringsOf(payloadParts(p, 0)), "")
	if p["conversation_id"] != "conv-1" || p["parent_message_id"] != "msg-end" || len(p["messages"].([]any)) != 1 || !strings.Contains(notice, "functions.exec") {
		t.Fatalf("ask = conversation %v parent %v messages %v", p["conversation_id"], p["parent_message_id"], p["messages"])
	}
	calls := outputOf(object, "function_call")
	if outputText(t, object) != "I will look it up first." || len(calls) != 1 || calls[0]["name"] != "lookup" {
		t.Fatalf("object = %v", object["output"])
	}
}

func TestReplyWithACallIsNotAskedAgain(t *testing.T) {
	payloads := make(chan map[string]any, 2)
	first := answerSnapshots(callAnswer, "{}") + execAttempt + turnEnd
	a, _ := newTestAdapter(t, lostCallUpstream(t, payloads, first, ""))
	object := forwardSession(t, a, testCredential(t, a, credentials{AccessToken: "AT"}), "", firstTurn)
	<-payloads
	if len(payloads) != 0 || len(outputOf(object, "function_call")) != 1 {
		t.Fatalf("asked again: %d more payloads, output %v", len(payloads), object["output"])
	}
}
