package main

// Choice and parallelism govern the next model response, not the history or
// schemas of an existing native Run. Canonicalize defaults for exact replays.
func nativePolicyChoice(choice any) any {
	if choice == nil || choice == "" {
		choice = "auto"
	}
	if forced := str(object(object(choice)["function"])["name"]); forced != "" {
		return map[string]any{"type": "function", "function": map[string]any{"name": forced}}
	}
	return choice
}

func nativePolicyDigest(chat chatRequest) string {
	parallel := chat.ParallelToolCalls == nil || *chat.ParallelToolCalls
	return digest(string(encodeJSON(map[string]any{"choice": nativePolicyChoice(chat.ToolChoice), "parallel_tool_calls": parallel})))
}

func nativeToolSchemaIdentity(tool chatTool) map[string]any {
	return map[string]any{"name": tool.Name, "schema": nativeSchemaContract(tool.Schema, false), "strict": tool.Strict}
}

func nativeToolSchemaDigest(tool chatTool) string {
	return digest(string(encodeJSON(nativeToolSchemaIdentity(tool))))
}

func (s *nativeSession) setToolPolicy(in nativeInput) {
	s.tools = nativeCloneTools(in.Tools)
	s.toolChoice = nativePolicyChoice(in.Chat.ToolChoice)
	s.parallelToolCalls = nil
	if in.Chat.ParallelToolCalls != nil {
		parallel := *in.Chat.ParallelToolCalls
		s.parallelToolCalls = &parallel
	}
}

// A retained Run can narrow policy, or re-enable tools from its original native
// catalog. It cannot learn an executable tool/schema that was never introduced
// at bootstrap merely because the caller changed tool_choice to auto.
func (s *nativeSession) checkToolPolicy(in nativeInput) error {
	for _, tool := range in.Tools {
		if signature, exists := s.nativeTools[tool.Name]; !exists || signature != nativeToolSchemaDigest(tool) {
			return nativeContinuationFailure("cursor_native_context_changed", "native_tool_set_changed", "native continuation would expand or change its original executable tool set; a new user turn is required")
		}
	}
	return nil
}
