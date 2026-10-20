package gateway

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"daydayup/internal/sessions"
)

const dshTitleSystem = "Create a concise title for an AI coding-assistant session from the supplied human messages."
const openCodeTitleSystem = "You are a title generator. You output ONLY a thread title. Nothing else."
const claudeTitleSystem = "Generate a concise, sentence-case title for this coding session. Return JSON with a single \"title\" field."

func jsonBody(value any) string { b, _ := json.Marshal(value); return string(b) }
func titleBody(question string, stream bool) string {
	input := jsonBody([]map[string]any{{"seq": 0, "text": question}})
	return jsonBody(map[string]any{"model": "m", "stream": stream, "messages": []map[string]any{{"role": "system", "content": dshTitleSystem}, {"role": "user", "content": dshTitleFrame + "\n" + input}}})
}
func mainBody(question string) string {
	return jsonBody(map[string]any{"model": "m", "messages": []map[string]any{{"role": "user", "content": question}}})
}
func titleResponse(text string) string {
	return jsonBody(map[string]any{"choices": []any{map[string]any{"message": map[string]string{"role": "assistant", "content": text}, "finish_reason": "stop"}}, "usage": map[string]int{"prompt_tokens": 7, "completion_tokens": 3, "total_tokens": 10}})
}

func TestTitleClassificationUsesInfrastructureInstructions(t *testing.T) {
	for _, tc := range []struct {
		name, body, header, detector string
		count                        int
	}{
		{"dsh", titleBody("修复登录", false), "", "dsh:session-title", 1},
		{"opencode", jsonBody(map[string]any{"messages": []any{map[string]string{"role": "system", "content": openCodeTitleSystem}, map[string]string{"role": "user", "content": openCodeTitleFrame + "\n"}, map[string]string{"role": "user", "content": "修复登录"}}}), "", "opencode:title-agent", 1},
		{"claude", jsonBody(map[string]any{"system": claudeTitleSystem, "messages": []any{map[string]string{"role": "user", "content": "<session>\n修复登录\n</session>"}}}), "", "claude-code:session-title", 1},
		{"explicit header", mainBody("修复登录"), "session-title", "header:x-request-purpose", 1},
		{"metadata", `{"metadata":{"purpose":"session-title"},"messages":[{"role":"user","content":"修复登录"}]}`, "", "body:metadata.purpose", 1},
		{"compatible dedicated system", `{"messages":[{"role":"system","content":"Generate a title for this conversation. Return only the title on a single line."},{"role":"user","content":"修复登录"}]}`, "", "compat:dedicated-title-prompt", 1},
		{"Chinese dedicated system", `{"messages":[{"role":"system","content":"为以下对话生成一个标题，只返回标题。"},{"role":"user","content":"修复登录"}]}`, "", "compat:dedicated-title-prompt", 1},
		{"article title", mainBody("给这篇文章生成标题"), "", "", 0},
		{"quoted system in user", mainBody(dshTitleSystem), "", "", 0},
		{"compaction", mainBody("总结对话"), "compaction", "", 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := httptest.NewRequest("POST", "/v1/chat/completions", nil)
			if tc.header != "" {
				r.Header.Set("X-Request-Purpose", tc.header)
			}
			title := inspectTitleRequest(r, "/v1/chat/completions", []byte(tc.body))
			if tc.detector == "" {
				if title != nil {
					t.Fatalf("ordinary request misclassified %+v", title)
				}
				return
			}
			if title == nil || title.Detector != tc.detector || len(title.SourceMessages) != tc.count || title.SourceMessages[0] != sessions.MessageHashes([]string{"修复登录"})[0] {
				t.Fatalf("wrong classification %+v", title)
			}
		})
	}
}

func TestHeaderlessTitleJoinsConversationRenamesAndCountsUsage(t *testing.T) {
	s, _ := newTestGateway(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		w.Header().Set("Content-Type", "application/json")
		if strings.Contains(string(b), dshTitleSystem) {
			io.WriteString(w, titleResponse("修复登录按钮"))
		} else {
			io.WriteString(w, sessionResponse)
		}
	}), 1)
	enableSessions(t, s)
	logs := captureLogs(s)
	first := sessionCall(s, "", "/v1/chat/completions", mainBody("登录按钮无法点击"))
	sid := first.Header().Get("X-Session-ID")
	w := sessionCall(s, "", "/v1/chat/completions", titleBody("登录按钮无法点击", false))
	if w.Code != 200 || w.Header().Get("X-Session-ID") != sid || w.Header().Get("X-Session-Source") != "title_context" || w.Header().Get("X-Request-Purpose") != "title" {
		t.Fatal("not associated", w.Code, w.Header())
	}
	summary, rows, total, err := s.Sessions.Detail(sid, 0, 30)
	if err != nil || total != 2 || summary.Name != "修复登录按钮" || summary.TitleRequests != 1 || summary.Usage.Total != 20 || summary.TitleUsage.Total != 10 || summary.NameInfo.Source != "model_title" || summary.NameInfo.RequestID != rows[0].ID {
		t.Fatalf("wrong title or usage %+v %+v %v", summary, rows, err)
	}
	if rows[0].Title == nil || rows[0].Title.Status != "applied" || rows[0].Title.Association != "user_messages" || rows[0].Title.LinkedRequestID != rows[1].ID || rows[0].AccountID != rows[1].AccountID {
		t.Fatalf("missing evidence %+v", rows)
	}
	if strings.Contains(logs.String(), "登录按钮") || !strings.Contains(logs.String(), `"request_purpose":"title"`) {
		t.Fatal("title leaked or purpose absent from logs")
	}
	s.Sessions, err = sessions.Open(s.Config.DBFile, "")
	if err != nil {
		t.Fatal(err)
	}
	summary, _, _, _ = s.Sessions.Detail(sid, 0, 30)
	if summary.Name != "修复登录按钮" || summary.NameInfo.Source != "model_title" {
		t.Fatal("title not persistent")
	}
	if err = s.Sessions.Update(sid, ptrString("手动名称"), nil); err != nil {
		t.Fatal(err)
	}
	w = sessionCall(s, "", "/v1/chat/completions", titleBody("登录按钮无法点击", false))
	summary, rows, _, _ = s.Sessions.Detail(sid, 0, 30)
	if w.Code != 200 || summary.Name != "手动名称" || summary.TitleRequests != 2 || summary.Usage.Total != 30 || rows[0].Title.Status != "manual_name_preserved" {
		t.Fatal("manual title replaced", summary, rows[0].Title)
	}
}
func ptrString(value string) *string { return &value }

func TestTitleFirstHeaderlessConversationKeepsOneArchive(t *testing.T) {
	s, _ := newTestGateway(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		io.WriteString(w, titleResponse("先到的标题"))
	}), 1)
	enableSessions(t, s)
	first := sessionCall(s, "", "/v1/chat/completions", titleBody("为标题先到的会话测试", false))
	sid := first.Header().Get("X-Session-ID")
	summary, rows, _, _ := s.Sessions.Detail(sid, 0, 30)
	if rows[0].Title.Status != "unassociated" || summary.Name != sid {
		t.Fatal("unassociated title applied")
	}
	// A pending title survives a restart and can then seed the main request.
	var err error
	s.Sessions, err = sessions.Open(s.Config.DBFile, "")
	if err != nil {
		t.Fatal(err)
	}
	w := sessionCall(s, "", "/v1/chat/completions", mainBody("为标题先到的会话测试"))
	if w.Code != 200 || w.Header().Get("X-Session-ID") != sid || w.Header().Get("X-Session-Source") != "title_seed" {
		t.Fatal("title seed lost", w.Header())
	}
	summary, rows, total, _ := s.Sessions.Detail(sid, 0, 30)
	if total != 2 || summary.Name != "先到的标题" || summary.Usage.Total != 20 || rows[1].Title.Status != "applied" || rows[1].Title.LinkedRequestID != rows[0].ID {
		t.Fatalf("wrong archive %+v %+v", summary, rows)
	}
}

func TestAmbiguousTitlesAreNotAttachedByModelOrTime(t *testing.T) {
	s, _ := newTestGateway(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		io.WriteString(w, titleResponse("不应乱关联"))
	}), 1)
	enableSessions(t, s)
	for _, sid := range []string{"one", "two"} {
		sessionCall(s, sid, "/v1/chat/completions", mainBody("相同用户输入"))
	}
	w := sessionCall(s, "", "/v1/chat/completions", titleBody("相同用户输入", false))
	sid := w.Header().Get("X-Session-ID")
	if sid == "one" || sid == "two" {
		t.Fatal("ambiguous title attached")
	}
	sum, rows, _, _ := s.Sessions.Detail(sid, 0, 30)
	if sum.Name != sid || rows[0].Title.Status != "unassociated" || rows[0].Title.Association != "ambiguous" {
		t.Fatalf("ambiguity not recorded %+v", rows[0].Title)
	}
	w = sessionCall(s, "one", "/v1/chat/completions", titleBody("相同用户输入", false))
	sum, _, _, _ = s.Sessions.Detail("one", 0, 30)
	if w.Code != 200 || sum.Name != "不应乱关联" {
		t.Fatal("explicit ID didn't resolve ambiguity")
	}
}

func TestTitleJSONAndSSEProtocols(t *testing.T) {
	for _, tc := range []struct{ name, path, wire, ct, want string }{
		{"chat JSON", "/v1/chat/completions", titleResponse(`{"title":"修复缓存"}`), "application/json", "修复缓存"},
		{"chat SSE", "/v1/chat/completions", "data: {\"choices\":[{\"delta\":{\"reasoning_content\":\"private reasoning\"}}]}\n\ndata: {\"choices\":[{\"delta\":{\"content\":\"修复\"}}]}\n\ndata: {\"choices\":[{\"delta\":{\"content\":\"缓存\"},\"finish_reason\":\"stop\"}],\"usage\":{\"prompt_tokens\":7,\"completion_tokens\":3,\"total_tokens\":10}}\n\ndata: [DONE]\n\n", "text/event-stream", "修复缓存"},
		{"messages JSON", "/v1/messages", `{"type":"message","content":[{"type":"thinking","thinking":"hidden"},{"type":"text","text":"修复缓存"}],"stop_reason":"end_turn","usage":{"input_tokens":7,"output_tokens":3}}`, "application/json", "修复缓存"},
		{"messages SSE", "/v1/messages", "data: {\"type\":\"content_block_start\",\"content_block\":{\"type\":\"text\",\"text\":\"\"}}\n\ndata: {\"type\":\"content_block_delta\",\"delta\":{\"type\":\"text_delta\",\"text\":\"修复缓存\"}}\n\ndata: {\"type\":\"message_delta\",\"delta\":{\"stop_reason\":\"end_turn\"}}\n\ndata: {\"type\":\"message_stop\"}\n\n", "text/event-stream", "修复缓存"},
		{"responses JSON", "/v1/responses", `{"status":"completed","output":[{"type":"message","content":[{"type":"output_text","text":"修复缓存"}]}],"usage":{"input_tokens":7,"output_tokens":3}}`, "application/json", "修复缓存"},
		{"responses SSE", "/v1/responses", "data: {\"type\":\"response.output_text.delta\",\"delta\":\"修复缓存\"}\n\ndata: {\"type\":\"response.completed\",\"response\":{\"status\":\"completed\",\"output\":[{\"type\":\"message\",\"content\":[{\"type\":\"output_text\",\"text\":\"修复缓存\"}]}]}}\n\n", "text/event-stream", "修复缓存"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s, _ := newTestGateway(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.Header.Get("X-Request-Purpose") != "" {
					t.Error("gateway purpose leaked")
				}
				w.Header().Set("Content-Type", tc.ct)
				io.WriteString(w, tc.wire)
			}), 1)
			enableSessions(t, s)
			body := mainBody("会话")
			if tc.path == "/v1/responses" {
				body = `{"model":"m","input":"会话"}`
			}
			w := identityCall(s, tc.path, body, map[string]string{"X-Session-ID": "known-session", "X-Request-Purpose": "session-title"})
			if w.Code != 200 || w.Body.String() != tc.wire {
				t.Fatal("wire changed", w.Code, w.Body)
			}
			sum, rows, _, _ := s.Sessions.Detail("known-session", 0, 30)
			if sum.Name != tc.want || rows[0].Title.Status != "applied" {
				t.Fatalf("not applied %+v %+v", sum, rows[0].Title)
			}
		})
	}
}

func TestInvalidFailedAndOrdinaryOutputDoNotRename(t *testing.T) {
	for _, tc := range []struct {
		name, wire string
		status     int
		purpose    bool
		want       string
	}{
		{"ordinary title-shaped output", titleResponse(`{"title":"不应该改名"}`), 200, false, ""},
		{"multiline", titleResponse("第一行\n第二行"), 200, true, "invalid_output"},
		{"long", titleResponse(strings.Repeat("长", 121)), 200, true, "invalid_output"},
		{"length stop", strings.Replace(titleResponse("截断标题"), `"stop"`, `"length"`, 1), 200, true, "invalid_output"},
		{"upstream failure", titleResponse("不应用错误标题"), 403, true, "failed"},
		{"queued response", `{"status":"queued","output_text":"未完成标题"}`, 200, true, "invalid_output"},
		{"cancelled response", `{"status":"cancelled","output_text":"取消后的标题"}`, 200, true, "invalid_output"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s, _ := newTestGateway(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(tc.status)
				io.WriteString(w, tc.wire)
			}), 1)
			enableSessions(t, s)
			h := map[string]string{"X-Session-ID": "keep-id"}
			if tc.purpose {
				h["X-Request-Purpose"] = "session-title"
			}
			identityCall(s, "/v1/chat/completions", mainBody("会话"), h)
			sum, rows, _, _ := s.Sessions.Detail("keep-id", 0, 30)
			if sum.Name != "keep-id" {
				t.Fatal("wrong rename", sum.Name)
			}
			if tc.want == "" {
				if rows[0].Title != nil {
					t.Fatal("normal reply inferred as title")
				}
			} else if rows[0].Title.Status != tc.want {
				t.Fatal("wrong status", rows[0].Title)
			}
		})
	}
}

func TestTitleCanAssociateWithRunningMainRequest(t *testing.T) {
	started, release := make(chan struct{}), make(chan struct{})
	var mainReply *httptest.ResponseRecorder
	s, _ := newTestGateway(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		w.Header().Set("Content-Type", "application/json")
		if strings.Contains(string(b), dshTitleSystem) {
			io.WriteString(w, titleResponse("并发标题"))
			return
		}
		close(started)
		<-release
		io.WriteString(w, sessionResponse)
	}), 1)
	enableSessions(t, s)
	done := make(chan struct{})
	go func() {
		defer close(done)
		mainReply = sessionCall(s, "", "/v1/chat/completions", mainBody("正在处理的任务"))
	}()
	<-started
	titleReply := sessionCall(s, "", "/v1/chat/completions", titleBody("正在处理的任务", false))
	close(release)
	<-done
	if titleReply.Code != 200 || titleReply.Header().Get("X-Session-ID") != mainReply.Header().Get("X-Session-ID") {
		t.Fatal("running main not matched")
	}
	sum, _, total, _ := s.Sessions.Detail(mainReply.Header().Get("X-Session-ID"), 0, 30)
	if total != 2 || sum.Name != "并发标题" || sum.Usage.Total != 20 {
		t.Fatal(sum)
	}
}
