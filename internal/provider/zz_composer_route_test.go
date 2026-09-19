package provider

import (
	"testing"
)

// TestCursorUsesChatServiceRoutes verifies that every cursor
// subscription model routes to the correct service:
//   - cursor/auto and cursor/default: AgentService (text path)
//   - composer-2.5, composer-2: ChatService (IDE composer family)
//   - gemini-3.8-flash: AgentService (text path — no built-in tools)
//   - tool-bearing requests: ChatService regardless of model
//   - -thinking models: ChatService
func TestCursorUsesChatServiceRoutes(t *testing.T) {
	// AgentService lane: cursor/auto and cursor/default without tools.
	// Sending these to ChatService would fail — they are text-only.
	if cursorUsesChatService("cursor/auto", []byte(`{}`)) {
		t.Fatal("cursor/auto must route to AgentService")
	}
	if cursorUsesChatService("cursor/default", []byte(`{}`)) {
		t.Fatal("cursor/default must route to AgentService")
	}
	// ChatService lane: IDE composer family. Sending these to
	// AgentService ends the turn with zero content (502).
	if !cursorUsesChatService("composer-2.5", []byte(`{}`)) {
		t.Fatal("composer-2.5 must route to ChatService")
	}
	if !cursorUsesChatService("composer-2", []byte(`{}`)) {
		t.Fatal("composer-2 must route to ChatService")
	}
	// gemini-3.8-flash: no built-in tools → AgentService (text path).
	if cursorUsesChatService("gemini-3.8-flash", []byte(`{}`)) {
		t.Fatal("gemini-3.8-flash without tools must route to AgentService")
	}
	// Non-composer models without tools still go to AgentService.
	if cursorUsesChatService("gpt-5.2", []byte(`{}`)) {
		t.Fatal("gpt-5.2 without tools must route to AgentService")
	}
	// Tool-bearing requests route to ChatService regardless of model.
	if !cursorUsesChatService("cursor/auto", []byte(`"tools""function"`)) {
		t.Fatal("tool-bearing cursor/auto must route to ChatService")
	}
	if !cursorUsesChatService("cursor/default", []byte(`"tools""function"`)) {
		t.Fatal("tool-bearing cursor/default must route to ChatService")
	}
	// -thinking models route to ChatService.
	if !cursorUsesChatService("gpt-5.2-thinking", []byte(`{}`)) {
		t.Fatal("-thinking model must route to ChatService")
	}
}
