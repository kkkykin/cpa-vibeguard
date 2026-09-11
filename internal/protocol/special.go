package protocol

import "errors"

func (d *Document) specialRequest() error {
	m := d.Body
	switch d.Format {
	case "openai-image", "openai-video":
		for _, key := range []string{"prompt", "negative_prompt"} {
			if err := d.str(m, key, "", false); err != nil {
				return err
			}
		}
	case "codex-alpha-search":
		if err := d.str(m, "query", "", false); err != nil {
			return err
		}
		if v, exists := m["queries"]; exists {
			a, err := array(v, "queries")
			if err != nil {
				return err
			}
			for i, v := range a {
				switch x := v.(type) {
				case string:
					d.Fields = append(d.Fields, Field{Path: pathIndex("/queries", i), Text: x, set: func(s string) { a[i] = s }})
				case map[string]any:
					if err := d.str(x, "query", pathIndex("/queries", i), true); err != nil {
						return err
					}
				default:
					return errors.New("invalid_search_query")
				}
			}
		}
	}
	return nil
}
func (d *Document) specialResponse() error {
	m := d.Body
	for _, key := range []string{"prompt", "revised_prompt", "text"} {
		if err := d.str(m, key, "", false); err != nil {
			return err
		}
	}
	for _, key := range []string{"data", "results"} {
		if v, exists := m[key]; exists {
			a, err := array(v, key)
			if err != nil {
				return err
			}
			for i, v := range a {
				x, err := object(v, key)
				if err != nil {
					return err
				}
				for _, field := range []string{"revised_prompt", "text", "title", "snippet"} {
					if err := d.str(x, field, pathIndex("/"+key, i), false); err != nil {
						return err
					}
				}
			}
		}
	}
	return nil
}
