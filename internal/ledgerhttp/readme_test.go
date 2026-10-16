package ledgerhttp_test

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"strconv"
	"strings"
	"testing"
)

var endpointLine = regexp.MustCompile(`^(GET|POST|PUT|PATCH|DELETE|HEAD|OPTIONS)\s+(/\S+)`)
var curlURL = regexp.MustCompile(`(?:https?://[^/\s]+|localhost:\d+)(/\S*)`)

func TestTheREADMEListsEveryMountedEndpointAndItsExamplesUseMountedPatterns(t *testing.T) {
	_, testFile, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("locate this test file")
	}
	dir := filepath.Dir(testFile)
	readme, err := os.ReadFile(filepath.Join(dir, "..", "..", "README.md"))
	if err != nil {
		t.Fatalf("read README.md: %v", err)
	}
	source, err := os.ReadFile(filepath.Join(dir, "ledgerhttp.go"))
	if err != nil {
		t.Fatalf("read ledgerhttp.go: %v", err)
	}

	mounted := mountedPatterns(t, source)
	listed := readmeEndpoints(string(readme))
	for pattern := range mounted {
		if _, ok := listed[pattern]; !ok {
			t.Errorf("mounted endpoint %q is missing from README.md's endpoint list", pattern)
		}
	}
	for pattern := range listed {
		if _, ok := mounted[pattern]; !ok {
			t.Errorf("README.md lists endpoint %q, which ledgerhttp does not mount", pattern)
		}
	}

	examples := consoleCurlRequests(string(readme))
	if len(examples) == 0 {
		t.Fatal("README.md has no curl requests in console blocks")
	}
	for _, example := range examples {
		if !matchesMountedPattern(mounted, example.method, example.path) {
			t.Errorf("README.md console curl %s %s does not match a mounted pattern", example.method, example.path)
		}
	}
}

func mountedPatterns(t *testing.T, source []byte) map[string]struct{} {
	t.Helper()
	file, err := parser.ParseFile(token.NewFileSet(), "ledgerhttp.go", source, 0)
	if err != nil {
		t.Fatalf("parse ledgerhttp.go: %v", err)
	}

	patterns := make(map[string]struct{})
	ast.Inspect(file, func(node ast.Node) bool {
		call, ok := node.(*ast.CallExpr)
		if !ok || len(call.Args) == 0 {
			return true
		}
		selector, ok := call.Fun.(*ast.SelectorExpr)
		if !ok || selector.Sel.Name != "HandleFunc" {
			return true
		}
		receiver, ok := selector.X.(*ast.Ident)
		if !ok || receiver.Name != "mux" {
			return true
		}
		literal, ok := call.Args[0].(*ast.BasicLit)
		if !ok || literal.Kind != token.STRING {
			return true
		}
		pattern, err := strconvUnquote(literal.Value)
		if err != nil {
			t.Errorf("unquote mounted pattern %s: %v", literal.Value, err)
			return true
		}
		patterns[pattern] = struct{}{}
		return true
	})
	return patterns
}

func strconvUnquote(value string) (string, error) {
	return strconv.Unquote(value)
}

func readmeEndpoints(readme string) map[string]struct{} {
	lines := strings.Split(readme, "\n")
	for i, line := range lines {
		if strings.TrimSpace(line) != "## The endpoints" {
			continue
		}
		for _, sectionLine := range lines[i+1:] {
			if strings.HasPrefix(strings.TrimSpace(sectionLine), "```") {
				return fencedEndpointPatterns(sectionLines(lines[i+1:]))
			}
		}
	}
	return map[string]struct{}{}
}

func sectionLines(lines []string) []string {
	var fenced []string
	inside := false
	for _, line := range lines {
		if strings.HasPrefix(strings.TrimSpace(line), "```") {
			if inside {
				return fenced
			}
			inside = true
			continue
		}
		if inside {
			fenced = append(fenced, line)
		}
	}
	return fenced
}

func fencedEndpointPatterns(lines []string) map[string]struct{} {
	patterns := make(map[string]struct{})
	for _, line := range lines {
		if match := endpointLine.FindStringSubmatch(line); match != nil {
			patterns[match[1]+" "+match[2]] = struct{}{}
		}
	}
	return patterns
}

type curlRequest struct {
	method string
	path   string
}

func consoleCurlRequests(readme string) []curlRequest {
	var requests []curlRequest
	insideConsole := false
	var command strings.Builder

	for _, line := range strings.Split(readme, "\n") {
		trimmed := strings.TrimSpace(line)
		if strings.HasPrefix(trimmed, "```") {
			if insideConsole {
				if command.Len() > 0 {
					requests = append(requests, parseCurlCommand(command.String())...)
					command.Reset()
				}
				insideConsole = false
			} else {
				insideConsole = trimmed == "```console"
			}
			continue
		}
		if !insideConsole {
			continue
		}
		if strings.HasPrefix(trimmed, "$ curl ") {
			if command.Len() > 0 {
				requests = append(requests, parseCurlCommand(command.String())...)
			}
			command.Reset()
			command.WriteString(strings.TrimPrefix(trimmed, "$ "))
			continue
		}
		if command.Len() > 0 {
			command.WriteByte(' ')
			command.WriteString(trimmed)
			if !strings.HasSuffix(trimmed, "\\") {
				requests = append(requests, parseCurlCommand(command.String())...)
				command.Reset()
			}
		}
	}
	if command.Len() > 0 {
		requests = append(requests, parseCurlCommand(command.String())...)
	}
	return requests
}

func parseCurlCommand(command string) []curlRequest {
	match := curlURL.FindStringSubmatch(command)
	if match == nil {
		return nil
	}
	method := "GET"
	fields := strings.Fields(command)
	for i := 0; i+1 < len(fields); i++ {
		if fields[i] == "-X" || fields[i] == "--request" {
			method = fields[i+1]
			break
		}
	}
	path, _, _ := strings.Cut(match[1], "?")
	return []curlRequest{{method: method, path: path}}
}

func matchesMountedPattern(patterns map[string]struct{}, method, path string) bool {
	for pattern := range patterns {
		mountedMethod, mountedPath, ok := strings.Cut(pattern, " ")
		if !ok || mountedMethod != method {
			continue
		}
		want, got := strings.Split(mountedPath, "/"), strings.Split(path, "/")
		if len(want) != len(got) {
			continue
		}
		matches := true
		for i := range want {
			if strings.HasPrefix(want[i], "{") && strings.HasSuffix(want[i], "}") {
				if got[i] == "" {
					matches = false
					break
				}
				continue
			}
			if want[i] != got[i] {
				matches = false
				break
			}
		}
		if matches {
			return true
		}
	}
	return false
}
