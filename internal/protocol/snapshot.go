package protocol

import "errors"

// Snapshot selects the fields in a complete stream component. It shares the
// ordinary response codecs but does not interpret a snapshot as another delta.
// The caller owns root; Marshal commits any string-encoded tool JSON to it.
func Snapshot(format, component string, root map[string]any, toolLimit int) (*Document, error) {
	d := &Document{Root: root, Body: root, Format: format, toolLimit: toolLimit}
	var err error
	switch component {
	case "chat_message":
		err = d.chatMessage(root, "", false)
	case "response":
		err = d.responsesResponse(root, "")
	case "response_item":
		err = d.responseItem(root, "")
	case "response_part":
		err = d.textPart(root, "", "responses")
	case "claude_message":
		err = d.claudeResponse()
	case "claude_block":
		err = d.claudeBlock(root, "")
	case "gemini_part":
		err = d.geminiPart(root, "")
	case "interaction":
		err = d.interactionsResponse(root, "")
	case "interaction_step":
		err = d.interactionStep(root, "")
	case "special":
		err = d.specialResponse()
	default:
		err = errors.New("unknown_snapshot_component")
	}
	if err != nil {
		return nil, err
	}
	if len(d.Fields) > MaxFields {
		return nil, errors.New("protocol_field_limit")
	}
	return d, nil
}
