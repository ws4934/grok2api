package inference

import (
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/chenyme/grok2api/backend/internal/application/gateway"
	mediadomain "github.com/chenyme/grok2api/backend/internal/domain/media"
	"github.com/chenyme/grok2api/backend/internal/infra/provider"
	"github.com/chenyme/grok2api/backend/internal/pkg/neterror"
	"github.com/gin-gonic/gin"
)

type screenshotModerationError struct {
	status  int
	message string
}

func (e screenshotModerationError) Error() string       { return e.message }
func (e screenshotModerationError) HTTPStatusCode() int { return e.status }
func (e screenshotModerationError) UpstreamDiagnostic() (string, provider.DiagnosticResponse) {
	return "", provider.DiagnosticResponse{StatusCode: e.status, Body: []byte(e.message)}
}

func TestScreenshotModerationErrorsRespectEachAPIEnvelope(t *testing.T) {
	gin.SetMode(gin.TestMode)
	for _, status := range []int{400, 403, 422} {
		for _, anthropic := range []bool{false, true} {
			router := gin.New()
			router.GET("/", func(c *gin.Context) {
				err := screenshotModerationError{status, "We’re so sorry, but the image we created may violate our content policies."}
				if anthropic {
					writeGatewayAnthropicError(c, err)
				} else {
					writeGatewayError(c, err)
				}
			})
			recorder := httptest.NewRecorder()
			router.ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, "/", nil))
			var value struct {
				Type  string `json:"type"`
				Error struct {
					Type, Code, Message string
					Param               any
				} `json:"error"`
			}
			if err := json.Unmarshal(recorder.Body.Bytes(), &value); err != nil {
				t.Fatal(err)
			}
			expectedType := "invalid_request_error"
			if anthropic && status == http.StatusForbidden {
				expectedType = "permission_error"
			}
			if recorder.Code != status || value.Error.Type != expectedType || value.Error.Message != provider.ContentPolicyMessage {
				t.Fatalf("status=%d anthropic=%v: %s", recorder.Code, anthropic, recorder.Body.String())
			}
			if anthropic && value.Type != "error" {
				t.Fatal(recorder.Body.String())
			}
			if !anthropic && (value.Error.Code != provider.ContentPolicyViolation || value.Error.Param != nil) {
				t.Fatal(recorder.Body.String())
			}
		}
	}
}

func TestHTTPErrorLocalizationPreservesMachineFieldsAndStatus(t *testing.T) {
	gin.SetMode(gin.TestMode)
	for _, status := range []int{403, 422} {
		for _, stream := range []bool{false, true} {
			for _, anthropic := range []bool{false, true} {
				router := gin.New()
				router.GET("/", func(c *gin.Context) {
					result := &gateway.Result{StatusCode: status, Header: http.Header{"Content-Type": {"application/json"}}, Body: io.NopCloser(strings.NewReader(`{"type":"error","error":{"type":"invalid_request_error","code":"provider_specific_code","param":"reference_images","message":"Generated video rejected by content moderation."}}`)), Finalize: func(gateway.Usage, string, string) {}}
					NewHandler(nil, nil, 1<<20).writeProtocolResult(c, result, stream, anthropic, streamProtocolChat, "")
				})
				recorder := httptest.NewRecorder()
				router.ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, "/", nil))
				var value struct {
					Error struct{ Code, Param, Type, Message string }
				}
				_ = json.Unmarshal(recorder.Body.Bytes(), &value)
				if recorder.Code != status || value.Error.Code != "provider_specific_code" || value.Error.Param != "reference_images" || value.Error.Type != "invalid_request_error" || value.Error.Message != provider.ContentPolicyMessage {
					t.Fatalf("stream=%v anthropic=%v: %d %s", stream, anthropic, recorder.Code, recorder.Body.String())
				}
			}
		}
	}
}

func TestClientErrorOmitsChannelsWithoutChangingProtocolOrDiagnostics(t *testing.T) {
	for _, raw := range []string{
		`{"error":{"message":"Grok Web 图片编辑不支持 mask","type":"invalid_request_error","code":"mask_unsupported","param":"mask"}}`,
		`{"error":{"message":"Grok Console 图片编辑不支持 mask","type":"invalid_request_error","code":"mask_unsupported","param":"mask"}}`,
	} {
		diagnostic := []byte(raw)
		localized := preserveHTTPErrorEnvelope(diagnostic, http.StatusBadRequest, false)
		var body struct {
			Error struct{ Message, Type, Code, Param string }
		}
		if err := json.Unmarshal(localized, &body); err != nil || body.Error.Message != "图片编辑不支持 mask" || body.Error.Type != "invalid_request_error" || body.Error.Code != "mask_unsupported" || body.Error.Param != "mask" {
			t.Fatalf("invalid protocol envelope: %s err=%v", localized, err)
		}
		// The raw diagnostic input is unchanged; only error.message is localized.
		if string(diagnostic) != raw {
			t.Fatal("diagnostic source was rewritten")
		}
	}
	generated := `{"data":[{"revised_prompt":"Grok Web 图片编辑不支持 mask","b64_json":"R3JvayBXZWI="}]}`
	if string(localizeErrorJSON([]byte(generated), http.StatusOK)) != generated {
		t.Fatal("successful image content was modified")
	}
}

func TestImageCallErrorLocalizationDoesNotRewriteOtherOutputItems(t *testing.T) {
	call := `{"type":"image_generation_call","id":"ig_test","status":"failed","result":null,"error":{"code":"moderation_blocked","type":"invalid_request_error","param":null,"message":"may violate our content policies"}}`
	for _, raw := range []string{
		`{"status":"failed","output":[` + call + `,{"type":"function_call","arguments":"{\"error\":\"keep me\"}"}]}`,
		`{"type":"response.output_item.done","sequence_number":9,"output_index":0,"item":` + call + `}`,
	} {
		got := string(localizeErrorJSON([]byte(raw), 200))
		if !strings.Contains(got, provider.ContentPolicyMessage) || !strings.Contains(got, `"code":"moderation_blocked"`) || strings.Contains(got, "content policies") {
			t.Fatal(got)
		}
		if strings.Contains(raw, "function_call") && !strings.Contains(got, `"arguments":"{\"error\":\"keep me\"}"`) {
			t.Fatal(got)
		}
	}
}

func TestCopyJSONStillRejectsEmptyUpstreamBody(t *testing.T) {
	recorder := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(recorder)
	_, err := copyJSON(c.Writer, strings.NewReader(""), streamProtocolChat)
	if !errors.Is(err, neterror.ErrUpstreamResponseEmpty) || recorder.Body.Len() != 0 {
		t.Fatalf("body=%s err=%v", recorder.Body.String(), err)
	}
}

type smallRead struct{ io.Reader }

func (r smallRead) Read(p []byte) (int, error) { return r.Reader.Read(p[:min(5, len(p))]) }

func TestStreamErrorLocalizationPreservesEventsCodesAndRawDiagnostics(t *testing.T) {
	gin.SetMode(gin.TestMode)
	for _, tc := range []struct {
		protocol streamProtocol
		code     string
		body     string
	}{
		{streamProtocolChat, "content_policy_violation", `data: {"error":{"type":"invalid_request_error","code":"content_policy_violation","param":"prompt","message":"Generated video rejected by content moderation."}}` + "\n\n"},
		{streamProtocolAnthropic, "content_policy_violation", "event: error\n" + `data: {"type":"error","error":{"type":"invalid_request_error","code":"content_policy_violation","param":"prompt","message":"Generated video rejected by content moderation."}}` + "\n\n"},
		{streamProtocolResponses, "image_content_policy_violation", "event: response.failed\n" + `data: {"type":"response.failed","sequence_number":9,"response":{"id":"resp_test","status":"failed","error":{"code":"image_content_policy_violation","message":"Generated video rejected by content moderation."}}}` + "\n\n"},
	} {
		recorder := httptest.NewRecorder()
		c, _ := gin.CreateTestContext(recorder)
		metadata, err := copyStream(c.Writer, smallRead{strings.NewReader(tc.body)}, tc.protocol, nil)
		if !errors.Is(err, errUpstreamStreamFailed) || !strings.Contains(recorder.Body.String(), provider.ContentPolicyMessage) || !strings.Contains(recorder.Body.String(), `"code":"`+tc.code+`"`) {
			t.Fatalf("protocol=%v output=%s err=%v", tc.protocol, recorder.Body.String(), err)
		}
		if metadata.StreamFailure == nil || !strings.Contains(string(metadata.StreamFailure.Body), "Generated video rejected by content moderation.") {
			t.Fatalf("raw diagnostic lost: %+v", metadata.StreamFailure)
		}
		if tc.protocol == streamProtocolResponses && !strings.Contains(recorder.Body.String(), `"sequence_number":9`) {
			t.Fatal(recorder.Body.String())
		}
		if tc.protocol != streamProtocolResponses && !strings.Contains(recorder.Body.String(), `"param":"prompt"`) {
			t.Fatal(recorder.Body.String())
		}
	}
}

func TestErrorLocalizationNeverChangesSuccessfulGeneratedContent(t *testing.T) {
	raw := `{"choices":[{"message":{"content":"Generated video rejected by content moderation.","tool_calls":[{"function":{"arguments":"{\"error\":\"keep me\"}"}}]}}]}`
	if got := string(localizeErrorJSON([]byte(raw), 200)); got != raw {
		t.Fatal(got)
	}
	failed := `{"id":"resp_failed","status":"failed","created_at":999999999999999999,"error":{"code":"image_content_policy_violation","message":"may violate our content policies"}}`
	recorder := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(recorder)
	_, err := copyJSON(c.Writer, strings.NewReader(failed), streamProtocolResponses)
	if err != nil || !strings.Contains(recorder.Body.String(), `999999999999999999`) || !strings.Contains(recorder.Body.String(), `"code":"image_content_policy_violation"`) || !strings.Contains(recorder.Body.String(), provider.ContentPolicyMessage) {
		t.Fatalf("%s %v", recorder.Body.String(), err)
	}
}

func TestMediaJobErrorsUseTheirNativeProtocol(t *testing.T) {
	job := mediadomain.Job{Status: mediadomain.StatusFailed, ErrorCode: provider.ContentPolicyViolation, ErrorMessage: provider.ContentPolicyMessage}
	video := videoGenerationResponse(job)
	if problem := video["error"].(gin.H); video["status"] != "failed" || problem["code"] != "invalid_argument" || problem["message"] != provider.ContentPolicyMessage {
		t.Fatal(video)
	}
	image := imageResponseObject(job, nil, nil)
	if problem := image["error"].(gin.H); image["status"] != "failed" || problem["code"] != "image_content_policy_violation" || problem["message"] != provider.ContentPolicyMessage {
		t.Fatal(image)
	}
	call := image["output"].([]any)[0].(gin.H)
	if problem := call["error"].(gin.H); problem["type"] != "invalid_request_error" || problem["code"] != provider.ContentPolicyViolation {
		t.Fatal(call)
	}
}

func TestImageResponseFailuresUseOfficialResponseErrorCodes(t *testing.T) {
	for _, tc := range []struct{ internal, official string }{
		{"generation_failed", "server_error"}, {"invalid_request", "invalid_prompt"},
		{"submission_uncertain", "server_error"}, {"rate_limited", "rate_limit_exceeded"},
	} {
		job := mediadomain.Job{Status: mediadomain.StatusFailed, ErrorCode: tc.internal, ErrorMessage: "English upstream detail"}
		problem := imageResponseObject(job, nil, nil)["error"].(gin.H)
		if problem["code"] != tc.official || strings.Contains(problem["message"].(string), "English") {
			t.Fatal(problem)
		}
	}
}
