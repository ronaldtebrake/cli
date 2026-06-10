//go:build ignore

// Command render-mermaid shows how `entire investigate` will render a Mermaid
// flowchart in the terminal: the indented outline, or a FALLBACK notice when
// the diagram isn't a flowchart it can render (in which case entire shows the
// raw Mermaid block instead).
//
// It accepts either a bare flowchart body or a fenced ```mermaid block, read
// from a file argument or stdin.
//
// Usage:
//
//	go run scripts/render-mermaid.go diagram.mmd
//	pbpaste | go run scripts/render-mermaid.go
//	go run scripts/render-mermaid.go <<'EOF'
//	flowchart LR
//	  A[Start] --> B[Done]
//	EOF
package main

import (
	"fmt"
	"io"
	"os"
	"strings"

	"github.com/entireio/cli/cmd/entire/cli/mermaidascii"
)

func main() {
	var (
		data []byte
		err  error
	)
	if len(os.Args) > 1 {
		data, err = os.ReadFile(os.Args[1])
	} else {
		data, err = io.ReadAll(os.Stdin)
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, "read input:", err)
		os.Exit(1)
	}

	body := stripFence(string(data))
	out, ok := mermaidascii.RenderFlowchart(body)
	if !ok {
		fmt.Println("FALLBACK: not a renderable flowchart — entire would show the raw Mermaid block.")
		os.Exit(2)
	}
	fmt.Println(out)
}

// stripFence removes a surrounding ```mermaid ... ``` fence if present, so the
// script accepts copy-pasted fenced blocks as well as bare flowchart bodies.
func stripFence(s string) string {
	lines := strings.Split(strings.TrimSpace(s), "\n")
	if len(lines) >= 2 && strings.HasPrefix(strings.TrimSpace(lines[0]), "```") {
		lines = lines[1:]
		if strings.TrimSpace(lines[len(lines)-1]) == "```" {
			lines = lines[:len(lines)-1]
		}
	}
	return strings.Join(lines, "\n")
}
