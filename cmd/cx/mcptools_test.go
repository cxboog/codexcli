package main

import (
	"os"
	"path/filepath"
	"reflect"
	"testing"
)

func TestSplitCommand(t *testing.T) {
	cases := map[string][]string{
		`npx -y @scope/server /tmp`:      {"npx", "-y", "@scope/server", "/tmp"},
		`  a   "b c"  'd "e"' f\ g ""  `: {"a", "b c", `d "e"`, "f g", ""},
		`srv --flag="x y"`:               {"srv", "--flag=x y"},
	}
	for in, want := range cases {
		got, err := splitCommand(in)
		if err != nil || !reflect.DeepEqual(got, want) {
			t.Errorf("splitCommand(%q) = %q, %v; want %q", in, got, err, want)
		}
	}
	if _, err := splitCommand(`a "b`); err == nil {
		t.Error("unterminated quote should fail")
	}
}

func TestLoadMCPConfig(t *testing.T) {
	t.Setenv("TOK", "secret")
	path := filepath.Join(t.TempDir(), "mcp.json")
	_ = os.WriteFile(path, []byte(`{"mcpServers":{
		"b":{"type":"http","url":"https://example.com/mcp","headers":{"Authorization":"Bearer ${TOK}"}},
		"a":{"command":"srv","args":["--x","$TOK"],"env":{"K":"v"}}}}`), 0o600)
	specs, err := parseMCPFlag("@" + path)
	if err != nil || len(specs) != 2 {
		t.Fatalf("specs=%+v err=%v", specs, err)
	}
	if a := specs[0]; a.name != "a" || a.command != "srv" || !reflect.DeepEqual(a.args, []string{"--x", "secret"}) || !reflect.DeepEqual(a.env, []string{"K=v"}) {
		t.Errorf("a = %+v", a)
	}
	if b := specs[1]; b.url != "https://example.com/mcp" || b.headers["Authorization"] != "Bearer secret" {
		t.Errorf("b = %+v", b)
	}
	if s, _ := parseMCPFlag("https://h/mcp"); len(s) != 1 || s[0].url == "" {
		t.Errorf("url form: %+v", s)
	}
}
