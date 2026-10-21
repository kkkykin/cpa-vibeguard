package protocol

// Tool results can contain the same typed content as messages, including MCP
// content arrays nested in a result object. Reuse the protocol's field codec
// so media payloads, URLs, filenames and node types remain opaque. Arbitrary
// JSON results still expose their string values; tool arguments are not
// interpreted as protocol nodes based on a coincidental "type" property.
func (d *Document) resultPart(m map[string]any, path string) (bool, error) {
	// Results may also be arbitrary business JSON. A coincidental type value
	// without the corresponding content member must keep the generic codec.
	if !resultContentMember(m) {
		return false, nil
	}
	switch d.Format {
	case "openai-response", "codex":
		switch kind(m) {
		case "text", "input_text", "output_text", "summary_text", "reasoning_text", "refusal",
			"image_url", "input_image", "image", "input_audio", "audio", "video", "video_url", "input_video", "input_file", "file":
			return true, d.textPart(m, path, "responses")
		}
	case "interactions":
		switch kind(m) {
		case "text", "thought_summary", "thinking", "reasoning",
			"image", "audio", "video", "document", "file", "image_url", "input_audio":
			return true, d.interactionPart(m, path)
		}
	}
	return false, nil
}

func resultContentMember(m map[string]any) bool {
	var keys []string
	switch kind(m) {
	case "text", "input_text", "output_text", "summary_text", "reasoning_text":
		keys = []string{"text"}
	case "refusal":
		keys = []string{"refusal"}
	case "thought_summary":
		keys = []string{"content", "text"}
	case "thinking", "reasoning":
		keys = []string{"thinking", "text"}
	case "input_image", "image_url":
		keys = []string{"image_url", "file_id"}
	case "input_file", "file":
		keys = []string{"file_data", "file_id", "file_url", "file", "data", "uri"}
	case "input_audio":
		keys = []string{"input_audio", "data"}
	case "image", "audio", "video", "document":
		keys = []string{"data", "source", "uri", "url"}
	case "video_url", "input_video":
		keys = []string{"video_url", "data", "file_id"}
	}
	for _, key := range keys {
		if _, exists := m[key]; exists {
			return true
		}
	}
	return false
}
