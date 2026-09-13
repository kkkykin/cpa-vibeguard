package protocol

import (
	"encoding/json"
	"errors"
)

func (d *Document) geminiRequest() error {
	m := d.Body
	if err := rejectKeys(m, "input", "messages", "prompt"); err != nil {
		return err
	}
	if v, exists := m["systemInstruction"]; exists {
		x, err := object(v, "systemInstruction")
		if err != nil {
			return err
		}
		if err := d.geminiContent(x, "/systemInstruction"); err != nil {
			return err
		}
	}
	a, err := array(m["contents"], "contents")
	if err != nil || len(a) == 0 {
		return errors.New("gemini_contents_required")
	}
	for i, v := range a {
		x, err := object(v, "contents")
		if err != nil {
			return err
		}
		if role, exists := x["role"]; exists && role != "user" && role != "model" {
			return errors.New("unsupported_gemini_role")
		}
		if err := d.geminiContent(x, pathIndex("/contents", i)); err != nil {
			return err
		}
	}
	if v, ok := m["tools"]; ok {
		a, err := array(v, "tools")
		if err != nil {
			return err
		}
		for i, v := range a {
			x, err := object(v, "tools")
			if err != nil {
				return err
			}
			if v, ok := x["functionDeclarations"]; ok {
				defs, err := array(v, "functionDeclarations")
				if err != nil {
					return err
				}
				for j, v := range defs {
					f, err := object(v, "functionDeclarations")
					if err != nil {
						return err
					}
					path := pathIndex(pathIndex("/tools", i)+"/functionDeclarations", j)
					if err := d.toolDefinition(f, path); err != nil {
						return err
					}
					for _, key := range []string{"response", "responseJsonSchema"} {
						if v, ok := f[key]; ok {
							if err := d.schema(v, path+"/"+key); err != nil {
								return err
							}
						}
					}
				}
			}
		}
	}
	if v, ok := m["generationConfig"]; ok {
		x, err := object(v, "generationConfig")
		if err != nil {
			return err
		}
		for _, key := range []string{"responseSchema", "responseJsonSchema"} {
			if v, ok := x[key]; ok {
				if err := d.schema(v, "/generationConfig/"+key); err != nil {
					return err
				}
			}
		}
	}
	return nil
}
func (d *Document) geminiContent(m map[string]any, path string) error {
	a, err := array(m["parts"], path+"/parts")
	if err != nil {
		return err
	}
	for i, v := range a {
		x, err := object(v, path+"/parts")
		if err != nil {
			return err
		}
		if err := d.geminiPart(x, pathIndex(path+"/parts", i)); err != nil {
			return err
		}
	}
	return nil
}
func (d *Document) geminiPart(m map[string]any, path string) error {
	primary := ""
	for _, key := range []string{"text", "inlineData", "fileData", "functionCall", "functionResponse", "executableCode", "codeExecutionResult"} {
		if _, exists := m[key]; exists {
			if primary != "" {
				return errors.New("mixed_gemini_part")
			}
			primary = key
		}
	}
	switch primary {
	case "text":
		return d.str(m, "text", path, true)
	case "inlineData", "fileData":
		_, err := object(m[primary], path+"/"+primary)
		return err
	case "functionCall":
		x, err := object(m[primary], path+"/"+primary)
		if err != nil {
			return err
		}
		return d.toolObject(x, "args", path+"/functionCall", true)
	case "functionResponse":
		x, err := object(m[primary], path+"/"+primary)
		if err != nil {
			return err
		}
		if err := d.toolObject(x, "response", path+"/functionResponse", true); err != nil {
			return err
		}
		if _, exists := x["parts"]; exists {
			return d.geminiContent(x, path+"/functionResponse")
		}
		return nil
	case "executableCode":
		x, err := object(m[primary], path+"/"+primary)
		if err != nil {
			return err
		}
		return d.str(x, "code", path+"/executableCode", true)
	case "codeExecutionResult":
		x, err := object(m[primary], path+"/"+primary)
		if err != nil {
			return err
		}
		return d.str(x, "output", path+"/codeExecutionResult", true)
	default:
		if _, exists := m["thoughtSignature"]; exists {
			return nil
		}
		return errors.New("unsupported_gemini_part")
	}
}
func (d *Document) geminiResponse() error {
	m := d.Body
	if v, exists := m["totalTokens"]; exists {
		if _, ok := v.(json.Number); !ok {
			return errors.New("invalid_token_count")
		}
		return nil
	}
	if v, exists := m["candidates"]; exists {
		a, err := array(v, "candidates")
		if err != nil {
			return err
		}
		for i, v := range a {
			x, err := object(v, "candidates")
			if err != nil {
				return err
			}
			if v, exists := x["content"]; exists {
				content, err := object(v, "content")
				if err != nil {
					return err
				}
				if err := d.geminiContent(content, pathIndex("/candidates", i)+"/content"); err != nil {
					return err
				}
			}
		}
	}
	if v, exists := m["promptFeedback"]; exists {
		x, err := object(v, "promptFeedback")
		if err != nil {
			return err
		}
		return d.str(x, "blockReasonMessage", "/promptFeedback", false)
	}
	return nil
}
