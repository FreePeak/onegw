package types

import "testing"

// Context-window overflow must be classified WITHOUT swallowing neighbouring
// 400 families: the GLM reasoning_effort 400 and quota/TPM 400s keep their
// terminal/shared-wall contracts respectively. The z.ai row is verbatim from
// the live incident (2026-09-12, `dev` combo, 283,915-token advisor request).
func TestContextWindowExceededClassification(t *testing.T) {
	cases := []struct {
		name string
		err  *APIError
		want bool
	}{
		{"z.ai observed 400",
			&APIError{Status: 400, Type: "BadRequestError",
				Message: "The input (283915 tokens) is longer than the model's context length (262144 tokens)."}, true},
		{"openai maximum context length",
			&APIError{Status: 400, Type: "invalid_request_error", Code: "context_length_exceeded",
				Message: "This model's maximum context length is 128000 tokens. However, your messages resulted in 150000 tokens."}, true},
		{"anthropic prompt too long",
			&APIError{Status: 400, Type: "invalid_request_error",
				Message: "prompt is too long: 250000 tokens > 200000 maximum"}, true},
		{"generic context window wording",
			&APIError{Status: 400, Message: "input is too long for the context window of this model"}, true},
		{"tencent hunyuan configured-limit wording (b-ai/hy3, live 2026-09-14)",
			&APIError{Status: 400, Type: "invalid_request_error",
				Message: "The request is invalid: Input tokens exceed the configured limit of 192000 tokens. Your messages resulted in 279229 tokens. Please reduce the length of the messages."}, true},
		{"same relay, singular-token short dialect (b-ai/qwen3.8-flash, live)",
			&APIError{Status: 400, Type: "invalid_request_error",
				Message: "Input token exceed the limit (request id: 20260914100012933380014c955d568H5kTRm8o)"}, true},

		// Negatives: these must NOT be treated as context overflow.
		{"glm effort 400 stays terminal",
			&APIError{Status: 400, Code: "1210", Message: "use low, high or max"}, false},
		{"tpm wall is not a context verdict",
			&APIError{Status: 429, Message: "The request rate exceeds the current model TPM limit 340000000."}, false},
		// The Hunyuan addition must stay scoped to the INPUT subject: an
		// output-cap refusal shares the "...tokens exceed the configured
		// limit" tail but indicts a parameter, not the window.
		{"max_tokens cap is not a context verdict",
			&APIError{Status: 400, Type: "invalid_request_error",
				Message: "max_tokens exceed the configured limit of 4096 for this deployment"}, false},
		{"rpm wall named as a limit is not a context verdict",
			&APIError{Status: 400, Type: "rate_limit_error",
				Message: "The request rate exceeds the current model RPM limit 1000."}, false},
		{"generic invalid_request keeps terminal contract",
			&APIError{Status: 400, Type: "invalid_request_error", Message: "messages: at least one message is required"}, false},
		{"reasoning echo refusal is its own class",
			&APIError{Status: 400, Message: "The `reasoning_content` in the thinking mode must be passed back to the API."}, false},
		{"nil receiver", nil, false},
	}
	for _, c := range cases {
		if got := c.err.ContextWindowExceeded(); got != c.want {
			t.Errorf("%s: ContextWindowExceeded()=%v want %v", c.name, got, c.want)
		}
	}
}
