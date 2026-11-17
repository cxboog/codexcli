package codex

import (
	"encoding/base64"
	"fmt"
	"mime"
	"net/http"
	"os"
	"path/filepath"
	"strings"
)

// Item is one entry of a Responses input or output list, kept as loose JSON
// so that anything the backend emits can be sent straight back as input on
// the next turn (reasoning items with their encrypted content included).
type Item map[string]any

// Tool is a tool definition, as loose JSON.
type Tool map[string]any

func (it Item) str(k string) string { s, _ := it[k].(string); return s }

// Type is the item's "type" (message, reasoning, function_call, ...).
func (it Item) Type() string { return it.str("type") }

// Role is the role of a message item.
func (it Item) Role() string { return it.str("role") }

// Text concatenates the text parts of a message item, or the summary of a
// reasoning item.
func (it Item) Text() string {
	var b strings.Builder
	key := "content"
	if it.Type() == "reasoning" {
		key = "summary"
	}
	parts, _ := it[key].([]any)
	for _, p := range parts {
		if m, ok := p.(map[string]any); ok {
			if s, ok := m["text"].(string); ok {
				if b.Len() > 0 && key == "summary" {
					b.WriteString("\n\n")
				}
				b.WriteString(s)
			}
		}
	}
	return b.String()
}

// FunctionCall is a function_call or custom_tool_call output item.
type FunctionCall struct {
	CallID    string
	Name      string
	Namespace string
	// Arguments is the JSON arguments string of a function call, or the raw
	// input of a custom (freeform) tool call.
	Arguments string
	Custom    bool
}

// AsFunctionCall views the item as a tool call the client must answer.
func (it Item) AsFunctionCall() (FunctionCall, bool) {
	switch it.Type() {
	case "function_call":
		return FunctionCall{CallID: it.str("call_id"), Name: it.str("name"), Namespace: it.str("namespace"), Arguments: it.str("arguments")}, true
	case "custom_tool_call":
		return FunctionCall{CallID: it.str("call_id"), Name: it.str("name"), Namespace: it.str("namespace"), Arguments: it.str("input"), Custom: true}, true
	}
	return FunctionCall{}, false
}

// Output builds the item that answers this call. output is a string, or a
// list of content parts (input_text / input_image maps) for rich results.
func (fc FunctionCall) Output(output any) Item {
	t := "function_call_output"
	if fc.Custom {
		t = "custom_tool_call_output"
	}
	return Item{"type": t, "call_id": fc.CallID, "output": output}
}

// Message builds a message item with one text part. role is "user",
// "developer", "system" or "assistant".
func Message(role, text string) Item {
	partType := "input_text"
	if role == "assistant" {
		partType = "output_text"
	}
	return Item{"type": "message", "role": role, "content": []any{
		map[string]any{"type": partType, "text": text},
	}}
}

// UserMessage builds a user message from text and optional image parts
// (see ImageFile, ImageURL).
func UserMessage(text string, images ...map[string]any) Item {
	content := make([]any, 0, len(images)+1)
	for _, im := range images {
		content = append(content, im)
	}
	if text != "" {
		content = append(content, map[string]any{"type": "input_text", "text": text})
	}
	return Item{"type": "message", "role": "user", "content": content}
}

// ImageURL builds an input_image part from an https or data: URL.
func ImageURL(url string) map[string]any {
	return map[string]any{"type": "input_image", "image_url": url}
}

// ImageBytes builds an input_image part from raw image data.
func ImageBytes(data []byte, mimeType string) map[string]any {
	if mimeType == "" {
		mimeType = http.DetectContentType(data)
	}
	return ImageURL("data:" + mimeType + ";base64," + base64.StdEncoding.EncodeToString(data))
}

// ImageFile builds an input_image part from a local file.
func ImageFile(path string) (map[string]any, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	mt := mime.TypeByExtension(strings.ToLower(filepath.Ext(path)))
	if !strings.HasPrefix(mt, "image/") {
		mt = http.DetectContentType(data)
	}
	if !strings.HasPrefix(mt, "image/") {
		return nil, fmt.Errorf("codex: %s does not look like an image (%s)", path, mt)
	}
	return ImageBytes(data, mt), nil
}

// FunctionTool defines a function the model may call. parameters is a JSON
// schema (any JSON-marshalable value); nil means no arguments.
func FunctionTool(name, description string, parameters any) Tool {
	if parameters == nil {
		parameters = map[string]any{"type": "object", "properties": map[string]any{}}
	}
	return Tool{"type": "function", "name": name, "description": description, "strict": false, "parameters": parameters}
}

// WebSearchTool lets the model search the web server-side. live=false
// restricts it to OpenAI's cached index.
func WebSearchTool(live bool) Tool {
	return Tool{"type": "web_search", "external_web_access": live}
}
